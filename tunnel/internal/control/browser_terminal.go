package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
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
	if c == nil || c.HTTP == nil || connectorprotocol.ValidateIdentifier(c.NodeID) != nil || connectorprotocol.ValidateOpaqueEpoch(c.ProcessEpoch) != nil || ticket == "" || origin == "" || host == "" {
		return BrowserTerminalAdmission{}, ErrControlInvalid
	}
	endpoint := c.HTTP.base.ResolveReference(&url.URL{Path: "/v1/edge/browser-terminal/control"})
	endpoint.Scheme = "wss"
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.HTTP.credential)
	header.Set("X-Paperboat-Edge-Node-ID", c.NodeID)
	header.Set("X-Paperboat-Edge-Process-Epoch", c.ProcessEpoch)
	client := *c.HTTP.client
	client.Timeout = 0 // the authorized terminal can outlive ordinary API timeouts
	dialCtx, stopDial := context.WithTimeout(ctx, 10*time.Second)
	connection, response, err := websocket.Dial(dialCtx, endpoint.String(), &websocket.DialOptions{HTTPClient: &client, HTTPHeader: header, CompressionMode: websocket.CompressionDisabled})
	stopDial()
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		return BrowserTerminalAdmission{}, ErrControlUnavailable
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
	firstCtx, stopFirst := context.WithTimeout(ctx, 15*time.Second)
	err = connection.Write(firstCtx, websocket.MessageText, request)
	var kind websocket.MessageType
	var raw []byte
	if err == nil {
		kind, raw, err = connection.Read(firstCtx)
	}
	stopFirst()
	if err != nil || kind != websocket.MessageText {
		slog.WarnContext(ctx, "browser terminal control admission failed", "close_code", websocket.CloseStatus(err))
		connection.CloseNow()
		return BrowserTerminalAdmission{}, ErrControlUnavailable
	}
	var reply struct {
		Credential        string `json:"credential"`
		TerminalSessionID string `json:"terminal_session_id"`
		AttachmentID      string `json:"attachment_id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&reply) != nil || decoder.Decode(new(any)) != io.EOF || reply.Credential == "" || connectorprotocol.ValidateIdentifier(reply.TerminalSessionID) != nil || connectorprotocol.ValidateIdentifier(reply.AttachmentID) != nil {
		connection.CloseNow()
		return BrowserTerminalAdmission{}, ErrControlUnavailable
	}
	done := make(chan struct{})
	var once sync.Once
	closeConnection := func() { once.Do(func() { connection.CloseNow() }) }
	go func() {
		defer close(done)
		defer closeConnection()
		for {
			readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			kind, message, err := connection.Read(readCtx)
			cancel()
			if err != nil || kind != websocket.MessageText || !bytes.Equal(message, []byte("ok")) {
				return
			}
		}
	}()
	return BrowserTerminalAdmission{Credential: reply.Credential, TerminalSessionID: reply.TerminalSessionID, AttachmentID: reply.AttachmentID, Closed: done, Close: closeConnection}, nil
}
