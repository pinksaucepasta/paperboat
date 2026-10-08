package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/connectorprotocol"
)

// BrowserTerminalAdmission is a live control-plane grant. Closing it releases
// the control plane's attachment reservation; Closed also fires on revocation,
// a failed policy check, or a missing heartbeat.
type BrowserTerminalAdmission struct {
	Credential        string
	TerminalSessionID string
	AssignmentID      string
	AttachmentID      string
	Closed            <-chan struct{}
	Close             func()
}

type BrowserTerminalClient struct {
	HTTP         *HTTPClient
	NodeID       string
	ProcessEpoch string
}

func (c *BrowserTerminalClient) Admit(ctx context.Context, ticket, origin, host string) (BrowserTerminalAdmission, error) {
	return c.admit(ctx, ticket, origin, host, false)
}
func (c *BrowserTerminalClient) admit(ctx context.Context, ticket, origin, host string, compare bool) (_ BrowserTerminalAdmission, resultErr error) {
	controlPath := "/v1/edge/browser-terminal/control"
	if compare {
		controlPath = "/v1/edge/browser-config-compare/control"
	}

	if ctx == nil || c == nil || c.HTTP == nil || connectorprotocol.ValidateIdentifier(c.NodeID) != nil || connectorprotocol.ValidateOpaqueEpoch(c.ProcessEpoch) != nil || ticket == "" || origin == "" || host == "" {
		return BrowserTerminalAdmission{}, ErrControlInvalid
	}
	ctx, finish := c.HTTP.startRequest(ctx, "connector_admission")
	defer func() { finish(resultErr) }()
	failure := func(category string, status int, cause error) error {
		return &RequestFailure{Path: controlPath, Category: category, Status: status, SupportReference: controlReference(ctx), Err: ErrControlUnavailable, Cause: cause}
	}
	endpoint := c.HTTP.base.ResolveReference(&url.URL{Path: controlPath})
	endpoint.Scheme = "wss"
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.HTTP.credential)
	header.Set("X-Paperboat-Edge-Node-ID", c.NodeID)
	header.Set("X-Paperboat-Edge-Process-Epoch", c.ProcessEpoch)
	c.HTTP.applyTraceHeaders(ctx, header)
	client := *c.HTTP.client
	client.Timeout = 0 // the authorized terminal can outlive ordinary API timeouts
	dialCtx, stopDial := context.WithTimeout(ctx, 10*time.Second)
	connection, response, err := websocket.Dial(dialCtx, endpoint.String(), &websocket.DialOptions{HTTPClient: &client, HTTPHeader: header, CompressionMode: websocket.CompressionDisabled})
	stopDial()
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
			if response.Body != nil {
				response.Body.Close()
			}
		}
		return BrowserTerminalAdmission{}, failure("transport", status, err)
	}
	connection.SetReadLimit(16 << 10)
	request, err := json.Marshal(struct {
		Ticket string `json:"ticket"`
		Origin string `json:"origin"`
		Host   string `json:"host"`
	}{ticket, origin, host})
	if err != nil {
		connection.CloseNow()
		return BrowserTerminalAdmission{}, ErrControlInvalid
	}
	defer clearControlBytes(request)
	firstCtx, stopFirst := context.WithTimeout(ctx, 15*time.Second)
	err = connection.Write(firstCtx, websocket.MessageText, request)
	var kind websocket.MessageType
	var raw []byte
	if err == nil {
		kind, raw, err = connection.Read(firstCtx)
	}
	stopFirst()
	if err != nil || kind != websocket.MessageText {
		connection.CloseNow()
		return BrowserTerminalAdmission{}, failure("transport", 0, err)
	}
	defer clearControlBytes(raw)
	var reply struct {
		Credential        string `json:"credential"`
		TerminalSessionID string `json:"terminal_session_id"`
		AssignmentID      string `json:"assignment_id"`
		AttachmentID      string `json:"attachment_id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&reply); err != nil {
		connection.CloseNow()
		return BrowserTerminalAdmission{}, failure("response_invalid", 0, err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		connection.CloseNow()
		return BrowserTerminalAdmission{}, failure("response_invalid", 0, err)
	}
	resourceValid := connectorprotocol.ValidateIdentifier(reply.TerminalSessionID) == nil && reply.AssignmentID == ""
	if compare {
		resourceValid = connectorprotocol.ValidateIdentifier(reply.AssignmentID) == nil && reply.TerminalSessionID == ""
	}
	if reply.Credential == "" || !resourceValid || connectorprotocol.ValidateIdentifier(reply.AttachmentID) != nil {
		connection.CloseNow()
		return BrowserTerminalAdmission{}, failure("response_invalid", 0, nil)
	}
	done := make(chan struct{})
	var once sync.Once
	var closing atomic.Bool
	closeConnection := func() {
		closing.Store(true)
		once.Do(func() { connection.CloseNow() })
	}
	go func() {
		defer close(done)
		defer closeConnection()
		for {
			readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			kind, message, err := connection.Read(readCtx)
			cancel()
			ok := err == nil && kind == websocket.MessageText && bytes.Equal(message, []byte("ok"))
			clearControlBytes(message)
			if !ok {
				if !closing.Load() && ctx.Err() == nil && websocket.CloseStatus(err) != websocket.StatusNormalClosure && c.HTTP.controlFailure != nil {
					status := 0
					if websocket.CloseStatus(err) == websocket.StatusPolicyViolation {
						status = http.StatusForbidden
					}
					category := "transport"
					if err == nil {
						category = "response_invalid"
					}
					c.HTTP.controlFailure(ctx, controlReference(ctx), failure(category, status, err))
				}
				return
			}
		}
	}()
	return BrowserTerminalAdmission{Credential: reply.Credential, TerminalSessionID: reply.TerminalSessionID, AssignmentID: reply.AssignmentID, AttachmentID: reply.AttachmentID, Closed: done, Close: closeConnection}, nil
}

type BrowserConfigCompareAdmission struct {
	Credential, AssignmentID, AttachmentID string
	Closed                                 <-chan struct{}
	Close                                  func()
}
type BrowserConfigCompareClient struct {
	HTTP                 *HTTPClient
	NodeID, ProcessEpoch string
}

func (c *BrowserConfigCompareClient) Admit(ctx context.Context, ticket, origin, host string) (BrowserConfigCompareAdmission, error) {
	if c == nil {
		return BrowserConfigCompareAdmission{}, ErrControlInvalid
	}
	out, err := (&BrowserTerminalClient{HTTP: c.HTTP, NodeID: c.NodeID, ProcessEpoch: c.ProcessEpoch}).admit(ctx, ticket, origin, host, true)
	return BrowserConfigCompareAdmission{Credential: out.Credential, AssignmentID: out.AssignmentID, AttachmentID: out.AttachmentID, Closed: out.Closed, Close: out.Close}, err
}
