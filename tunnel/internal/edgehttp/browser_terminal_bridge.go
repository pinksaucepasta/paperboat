package edgehttp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"

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
	admissionWatcherDone := make(chan struct{})
	defer func() { cancel(); <-admissionWatcherDone }()
	go func() {
		defer close(admissionWatcherDone)
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
		p.observeBrowserTerminal(ctx, err)
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		http.Error(w, "Browser terminal host is unavailable", http.StatusBadGateway)
		return
	}
	if upstream.Subprotocol() != browserTerminalSubprotocol {
		p.observeBrowserTerminal(ctx, ErrBrowserTerminalMessage)
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
		p.observeBrowserTerminal(ctx, err)
		return
	}
	browser.SetReadLimit(browserTerminalMaxRecordBytes + 1)
	defer browser.CloseNow()
	p.observeBrowserTerminal(ctx, bridgeBrowserTerminal(ctx, browser, upstream, subscription))
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

func bridgeBrowserTerminal(ctx context.Context, browser, upstream *websocket.Conn, subscription *BrowserTerminalSubscription) (result error) {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	failures := make(chan error, 3)
	defer func() {
		cancel()
		_ = browser.CloseNow()
		_ = upstream.CloseNow()
		workers.Wait()
		close(failures)
		for cause := range failures {
			result = errors.Join(result, cause)
		}
	}()
	outbound := make(chan []byte, browserTerminalBridgeQueue)
	report := func(err error) {
		if requestCanceled(ctx, err) {
			return
		}
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

	workers.Add(1)
	go func() {
		defer workers.Done()
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

	workers.Add(1)
	go func() {
		defer workers.Done()
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

	workers.Add(1)
	go func() {
		defer workers.Done()
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
			return ctx.Err()
		case <-subscription.Done():
			return ErrBrowserTerminalHubEnded
		case cause := <-failures:
			return cause
		case message := <-outbound:
			if err := browser.Write(ctx, websocket.MessageBinary, message); err != nil {
				return err
			}
		}
	}
}

// Expected shutdown must account for every leaf so a simultaneous operational
// failure is never hidden by a normal participant departure.
func browserTerminalExpected(err error) bool {
	return requestErrorLeaves(err, func(leaf error) bool {
		if leaf == context.Canceled || leaf == ErrBrowserTerminalHubEnded {
			return true
		}
		switch closed := leaf.(type) {
		case websocket.CloseError:
			return closed.Code == websocket.StatusNormalClosure || closed.Code == websocket.StatusGoingAway
		case *websocket.CloseError:
			return closed != nil && (closed.Code == websocket.StatusNormalClosure || closed.Code == websocket.StatusGoingAway)
		}
		return false
	}, true)
}

func (p *Policy) observeBrowserTerminal(ctx context.Context, err error) {
	if err != nil && p.config.OnFailure != nil && !browserTerminalExpected(err) {
		p.config.OnFailure(ctx, err)
	}
}
