package edgehttp

import (
	"context"
	"net/http"
	"net/url"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/route"
)

const browserTerminalBridgeQueue = 8

func (p *Policy) serveBrowserTerminal(w http.ResponseWriter, request *http.Request, host string, match route.RouteMatch, admission control.BrowserTerminalAdmission) {
	if admission.Close == nil || admission.Closed == nil || admission.Credential == "" {
		if admission.Close != nil {
			admission.Close()
		}
		http.Error(w, "Browser terminal admission unavailable", http.StatusServiceUnavailable)
		return
	}
	defer admission.Close()
	key := BrowserTerminalSessionKey{
		RouteID:           match.Rule.RouteID,
		TerminalSessionID: admission.TerminalSessionID,
		ProcessGeneration: match.Rule.ConnectorProcessGeneration,
	}
	fence := browserTerminalFenceForMatch(match)
	subscription, err := p.config.BrowserTerminalHub.Subscribe(key, fence, admission.AttachmentID)
	if err != nil {
		status := http.StatusServiceUnavailable
		if err == ErrBrowserTerminalHubFull {
			status = http.StatusTooManyRequests
		}
		http.Error(w, "Browser terminal is at capacity", status)
		return
	}
	defer subscription.Close()

	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	go func() {
		select {
		case <-admission.Closed:
			cancel()
		case <-ctx.Done():
		}
	}()

	upstreamURL := (&url.URL{Scheme: "wss", Host: host, Path: "/v1/browser-terminal"}).String()
	upstreamHeader := make(http.Header)
	upstreamHeader.Set("Authorization", "Bearer "+admission.Credential)
	upstreamClient := &http.Client{Transport: p.config.RuntimeCarrierTransport}
	upstream, response, err := websocket.Dial(ctx, upstreamURL, &websocket.DialOptions{
		HTTPClient:      upstreamClient,
		HTTPHeader:      upstreamHeader,
		Subprotocols:    []string{browserTerminalSubprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		http.Error(w, "Browser terminal host is unavailable", http.StatusBadGateway)
		return
	}
	if upstream.Subprotocol() != browserTerminalSubprotocol {
		upstream.CloseNow()
		http.Error(w, "Browser terminal host rejected the protocol", http.StatusBadGateway)
		return
	}
	upstream.SetReadLimit(browserTerminalMaxRecordBytes)
	defer upstream.CloseNow()

	// The one-use control admission has already bound the browser Origin to the
	// ticket. The edge terminates this WebSocket only to add its outer message
	// discriminator; all inner TLS and shared-output bytes remain opaque.
	browser, err := websocket.Accept(w, request, &websocket.AcceptOptions{
		Subprotocols:       []string{browserTerminalSubprotocol},
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	browser.SetReadLimit(browserTerminalMaxRecordBytes + 1)
	defer browser.CloseNow()
	bridgeBrowserTerminal(ctx, browser, upstream, subscription)
}

func browserTerminalFenceForMatch(match route.RouteMatch) BrowserTerminalRouteFence {
	rule := match.Rule
	return BrowserTerminalRouteFence{
		Identity: datacarrier.Identity{
			AccountID:         rule.AccountID,
			HostID:            rule.HostID,
			TunnelID:          rule.TunnelID,
			ConnectorID:       rule.ConnectorID,
			SessionID:         rule.ConnectorSessionID,
			ProcessGeneration: rule.ConnectorProcessGeneration,
			Generation:        rule.ConfigGeneration,
		},
		Revision:             rule.Revision,
		AttachmentGeneration: rule.Generation,
	}
}

func bridgeBrowserTerminal(ctx context.Context, browser, upstream *websocket.Conn, subscription *BrowserTerminalSubscription) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	outbound := make(chan []byte, browserTerminalBridgeQueue)
	failures := make(chan error, 1)
	report := func(err error) {
		select {
		case failures <- err:
		default:
		}
	}
	queue := func(message []byte) error {
		select {
		case outbound <- message:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		default:
			return ErrBrowserTerminalHubFull
		}
	}

	go func() {
		for {
			kind, message, err := browser.Read(ctx)
			if err != nil {
				report(err)
				return
			}
			if kind != websocket.MessageBinary {
				report(ErrBrowserTerminalMessage)
				return
			}
			payload, err := browserTerminalTLSMessagePayload(message)
			if err != nil {
				report(err)
				return
			}
			if err := upstream.Write(ctx, websocket.MessageBinary, payload); err != nil {
				report(err)
				return
			}
		}
	}()

	go func() {
		for {
			kind, payload, err := upstream.Read(ctx)
			if err != nil {
				report(err)
				return
			}
			if kind != websocket.MessageBinary || len(payload) == 0 || len(payload) > browserTerminalMaxRecordBytes {
				report(ErrBrowserTerminalMessage)
				return
			}
			message, err := browserTerminalTLSMessage(payload)
			if err != nil {
				report(err)
				return
			}
			if err := queue(message); err != nil {
				report(err)
				return
			}
		}
	}()

	go func() {
		for {
			record, err := subscription.Next(ctx)
			if err != nil {
				report(err)
				return
			}
			message, err := browserTerminalSharedOutputMessagePayload(record)
			if err != nil {
				report(err)
				return
			}
			// The hub owns the bounded per-browser backlog. Wait for the
			// writer here so an ordinary burst cannot fill the small bridge
			// multiplexing queue and force a fresh screen.
			select {
			case outbound <- message:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-subscription.Done():
			return
		case <-failures:
			cancel()
			return
		case message := <-outbound:
			if err := browser.Write(ctx, websocket.MessageBinary, message); err != nil {
				cancel()
				return
			}
		}
	}
}
