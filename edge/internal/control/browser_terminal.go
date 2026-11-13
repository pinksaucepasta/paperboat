package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/strictjson"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/shared/terminalend"
)

// BrowserTerminalAdmission is a live control-plane grant. Closing it releases
// the control plane's attachment reservation; Closed also fires on revocation,
// a failed policy check, or a missing heartbeat.
// BrowserTerminalRefused: the control plane refused the admission and said why (the reason the server closed the
// control channel with). The edge closes the browser's WebSocket with that reason.
type BrowserTerminalRefused struct{ Reason terminalend.Reason }

func (e *BrowserTerminalRefused) Error() string {
	return "browser terminal refused: " + string(e.Reason)
}

// controlReason is the reason a control channel close names. A server that names none the edge knows (an older
// build) is "edge_unavailable": the browser retries shortly.
func controlReason(closed websocket.CloseError) terminalend.Reason {
	if reason := terminalend.Parse(closed.Reason); reason != terminalend.Protocol || closed.Reason == string(terminalend.Protocol) {
		return reason
	}
	return terminalend.EdgeUnavailable
}

type BrowserTerminalAdmission struct {
	Credential        string
	TerminalSessionID string
	AttachmentID      string
	Closed            <-chan struct{}
	Close             func()
	// EndReason is why the control plane ended the admission once Closed has fired: the reason it closed the control
	// channel with, or "" when the edge closed it or the connection failed.
	EndReason func() terminalend.Reason
}

type BrowserTerminalClient struct {
	HTTP         *HTTPClient
	NodeID       string
	ProcessEpoch string
}

func (c *BrowserTerminalClient) Admit(ctx context.Context, ticket, origin, host string) (BrowserTerminalAdmission, error) {
	return c.admit(ctx, ticket, origin, host)
}

// controlResumeGrace bounds how long a cut control channel is resumed before the terminal ends: the
// server's authorityOutageGrace, which keeps an attachment through an unanswered check that long.
var controlResumeGrace = 6 * time.Second

type browserTerminalControlReply struct {
	Credential        string `json:"credential"`
	TerminalSessionID string `json:"terminal_session_id"`
	AttachmentID      string `json:"attachment_id"`
}

func (c *BrowserTerminalClient) admit(ctx context.Context, ticket, origin, host string) (_ BrowserTerminalAdmission, resultErr error) {
	if ctx == nil || c == nil || c.HTTP == nil || connectorprotocol.ValidateIdentifier(c.NodeID) != nil || connectorprotocol.ValidateOpaqueEpoch(c.ProcessEpoch) != nil || ticket == "" || origin == "" || host == "" {
		return BrowserTerminalAdmission{}, ErrControlInvalid
	}
	ctx, finish := c.HTTP.startRequest(ctx, "connector_admission")
	defer func() { finish(resultErr) }()
	connection, reply, err := c.openControl(ctx, ticket, origin, host, "")
	if err != nil {
		return BrowserTerminalAdmission{}, err
	}
	if reply.Credential == "" || connectorprotocol.ValidateIdentifier(reply.TerminalSessionID) != nil || connectorprotocol.ValidateIdentifier(reply.AttachmentID) != nil {
		connection.CloseNow()
		return BrowserTerminalAdmission{}, c.failure(ctx, "response_invalid", 0, nil)
	}
	done := make(chan struct{})
	var mu sync.Mutex
	closed := false
	var endReason atomic.Value // terminalend.Reason
	closeConnection := func() {
		mu.Lock()
		defer mu.Unlock()
		if !closed {
			closed = true
			connection.CloseNow()
		}
	}
	isClosing := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return closed
	}
	go func() {
		defer close(done)
		defer closeConnection()
		var resumedAt time.Time
		for {
			mu.Lock()
			current := connection
			mu.Unlock()
			err := readHeartbeats(ctx, current)
			var ended websocket.CloseError
			if errors.As(err, &ended) && ended.Code == websocket.StatusPolicyViolation {
				// The server ended the admission with a reason (revoked, ended, policy): an answer.
				endReason.Store(controlReason(ended))
				return
			}
			if isClosing() || ctx.Err() != nil || websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				return
			}
			// Cut without an answer: a server rollout's proxy closes the socket (Caddy
			// stream_close_delay, close 1001) or the server went away. The browser stream is still up;
			// resume the same attachment, which the server admits again only if it still may.
			// A channel cut again within the grace of its last resume is not a rollout: the terminal
			// ends instead of resuming without bound.
			var next *websocket.Conn
			resumeErr := errBrowserTerminalControlFlapping
			if resumedAt.IsZero() || time.Since(resumedAt) >= controlResumeGrace {
				next, resumeErr = c.resumeControl(ctx, ticket, origin, host, reply.AttachmentID)
				resumedAt = time.Now()
			}
			var refusal *BrowserTerminalRefused
			if errors.As(resumeErr, &refusal) {
				endReason.Store(refusal.Reason)
				return
			}
			if resumeErr != nil {
				if !isClosing() && ctx.Err() == nil && c.HTTP.controlFailure != nil {
					c.HTTP.controlFailure(ctx, controlReference(ctx), c.failure(ctx, "resume", 0, errors.Join(err, resumeErr)))
				}
				return
			}
			mu.Lock()
			if closed {
				mu.Unlock()
				next.CloseNow()
				return
			}
			connection = next
			mu.Unlock()
		}
	}()
	return BrowserTerminalAdmission{Credential: reply.Credential, TerminalSessionID: reply.TerminalSessionID, AttachmentID: reply.AttachmentID, Closed: done, Close: closeConnection, EndReason: func() terminalend.Reason {
		reason, _ := endReason.Load().(terminalend.Reason)
		return reason
	}}, nil
}

// readHeartbeats reads the server's "ok" every check (2 s) until the channel stops being healthy.
func readHeartbeats(ctx context.Context, connection *websocket.Conn) error {
	for {
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		kind, message, err := connection.Read(readCtx)
		cancel()
		ok := err == nil && kind == websocket.MessageText && bytes.Equal(message, []byte("ok"))
		clearControlBytes(message)
		if !ok {
			if err == nil {
				err = errBrowserTerminalHeartbeat
			}
			return err
		}
	}
}

var (
	errBrowserTerminalHeartbeat       = errors.New("browser terminal control sent something other than a heartbeat")
	errBrowserTerminalControlFlapping = errors.New("browser terminal control was cut again right after its resume")
)

// resumeControl reopens a cut control channel for attachment within controlResumeGrace, with backoff
// between attempts. A refusal is final; the last failure is returned when the grace runs out.
// Prior art: tailscale control/controlclient/auto.go re-establishes its control stream with its
// durable identity after a backoff instead of treating one cut as the end.
func (c *BrowserTerminalClient) resumeControl(ctx context.Context, ticket, origin, host, attachment string) (*websocket.Conn, error) {
	graceCtx, cancel := context.WithTimeout(ctx, controlResumeGrace)
	defer cancel()
	backoff := 100 * time.Millisecond
	for {
		connection, reply, err := c.openControl(graceCtx, ticket, origin, host, attachment)
		if err == nil && reply.AttachmentID == attachment {
			return connection, nil
		}
		if err == nil {
			connection.CloseNow()
			return nil, &BrowserTerminalRefused{Reason: terminalend.Protocol}
		}
		var refusal *BrowserTerminalRefused
		if errors.As(err, &refusal) {
			return nil, err
		}
		timer := time.NewTimer(backoff)
		select {
		case <-graceCtx.Done():
			timer.Stop()
			return nil, err
		case <-timer.C:
		}
		backoff = min(backoff*2, time.Second)
	}
}

func (c *BrowserTerminalClient) failure(ctx context.Context, category string, status int, cause error) error {
	return &RequestFailure{Path: browserTerminalControlPath, Category: category, Status: status, SupportReference: controlReference(ctx), Err: ErrControlUnavailable, Cause: cause}
}

const browserTerminalControlPath = "/v1/edge/browser-terminal/control"

// openControl dials the control socket, sends the admission (or the resume of attachment) and reads
// the server's reply. A policy-violation close is the server refusing with a reason.
func (c *BrowserTerminalClient) openControl(ctx context.Context, ticket, origin, host, attachment string) (*websocket.Conn, browserTerminalControlReply, error) {
	endpoint := c.HTTP.base.ResolveReference(&url.URL{Path: browserTerminalControlPath})
	endpoint.Scheme = "wss"
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.HTTP.credential)
	header.Set("X-Paperboat-Edge-Node-ID", c.NodeID)
	header.Set("X-Paperboat-Edge-Process-Epoch", c.ProcessEpoch)
	c.HTTP.applyTraceHeaders(ctx, header)
	client := *c.HTTP.client
	client.Timeout = 0 // the authorized terminal can outlive ordinary API timeouts
	// The ticket travels only after the upgrade, so a handshake a rollout cut is dialed again.
	var connection *websocket.Conn
	var response *http.Response
	var err error
	for attempt := 1; ; attempt++ {
		dialCtx, stopDial := context.WithTimeout(ctx, 10*time.Second)
		connection, response, err = websocket.Dial(dialCtx, endpoint.String(), &websocket.DialOptions{HTTPClient: &client, HTTPHeader: header, CompressionMode: websocket.CompressionDisabled})
		stopDial()
		if err == nil || response != nil || !resendAfterCut(ctx, attempt, err) {
			break
		}
	}
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
			if response.Body != nil {
				response.Body.Close()
			}
		}
		return nil, browserTerminalControlReply{}, c.failure(ctx, "dial", status, err)
	}
	connection.SetReadLimit(16 << 10)
	request, err := json.Marshal(struct {
		Ticket string `json:"ticket"`
		Origin string `json:"origin"`
		Host   string `json:"host"`
		Resume string `json:"resume,omitempty"`
	}{ticket, origin, host, attachment})
	if err != nil {
		connection.CloseNow()
		return nil, browserTerminalControlReply{}, ErrControlInvalid
	}
	defer clearControlBytes(request)
	firstCtx, stopFirst := context.WithTimeout(ctx, 15*time.Second)
	stage := "first_write"
	err = connection.Write(firstCtx, websocket.MessageText, request)
	var kind websocket.MessageType
	var raw []byte
	if err == nil {
		stage = "first_read"
		kind, raw, err = connection.Read(firstCtx)
	}
	stopFirst()
	var closed websocket.CloseError
	if errors.As(err, &closed) && closed.Code == websocket.StatusPolicyViolation {
		connection.CloseNow()
		return nil, browserTerminalControlReply{}, &BrowserTerminalRefused{Reason: controlReason(closed)}
	}
	if err != nil || kind != websocket.MessageText {
		connection.CloseNow()
		return nil, browserTerminalControlReply{}, c.failure(ctx, stage, 0, err)
	}
	defer clearControlBytes(raw)
	var reply browserTerminalControlReply
	if err := strictjson.DecodeResponse(raw, &reply, 16); err != nil {
		connection.CloseNow()
		return nil, browserTerminalControlReply{}, c.failure(ctx, "response_invalid", 0, err)
	}
	return connection, reply, nil
}
