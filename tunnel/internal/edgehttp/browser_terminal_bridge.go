package edgehttp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/shared/terminalend"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/route"
)

const browserTerminalBridgeQueue = 8

// serveBrowserTerminal accepts the browser's WebSocket first, then admits the one-use ticket with the control plane.
// Everything that can fail afterwards ends in a close the browser can read (a 4xxx code and a typed reason), where a
// failed upgrade is an HTTP status JavaScript cannot see and the console could only retry blindly. Nothing is
// forwarded before the control plane admits the ticket.
func (p *Policy) serveBrowserTerminal(w http.ResponseWriter, request *http.Request, host string, match route.RouteMatch, ticket string) {
	browser, err := websocket.Accept(w, request, &websocket.AcceptOptions{
		Subprotocols: []string{browserTerminalSubprotocol},
		// The one-use control admission binds the browser Origin to the ticket; the edge terminates this WebSocket
		// only to add its outer message discriminator, and all inner TLS and shared-output bytes stay opaque.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		p.observeBrowserTerminal(request.Context(), err)
		return
	}
	browser.SetReadLimit(browserTerminalMaxRecordBytes + 1)
	defer browser.CloseNow()
	refuse := func(reason terminalend.Reason, cause error) {
		p.observeBrowserTerminal(request.Context(), browserTerminalRefusal{reason: reason, cause: cause})
		_ = browser.Close(websocket.StatusCode(reason.CloseCode()), string(reason))
	}

	admission, err := p.config.BrowserTerminal.Admit(request.Context(), ticket, request.Header.Get("Origin"), host)
	if err != nil {
		var refused *control.BrowserTerminalRefused
		if errors.As(err, &refused) {
			refuse(refused.Reason, err)
		} else {
			refuse(terminalend.EdgeUnavailable, err)
		}
		return
	}
	if admission.Close == nil || admission.Closed == nil || admission.Credential == "" {
		if admission.Close != nil {
			admission.Close()
		}
		refuse(terminalend.EdgeUnavailable, errBrowserTerminalAdmissionIncomplete)
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
		// A stale fence or a closed hub is a carrier that was replaced under the ticket: the machine is
		// reconnecting. Full is this edge's participant cap.
		if errors.Is(err, ErrBrowserTerminalHubFull) {
			refuse(terminalend.TooManyPending, err)
		} else {
			refuse(terminalend.MachineUnavailable, err)
		}
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

	// The browser's upstream stream is accounted to its attachment, which the server records
	// as the session's usage authority when it admits the ticket, and is closed at the cap.
	environment := match.Rule.Environment
	if environment == "" && p.config.HelperAccess != nil {
		// A runtime route's rule carries no environment; the server-signed host credential
		// of this admission names the machine's environment.
		if claims, err := p.config.HelperAccess.VerifyHelperAccess(ctx, admission.Credential); err == nil {
			environment = claims.EnvironmentID
		}
	}
	dialCtx := browserTerminalUsage(ctx, environment, admission, time.Now().UTC())
	upstreamURL := (&url.URL{Scheme: "wss", Host: host, Path: "/v1/browser-terminal"}).String()
	upstreamHeader := make(http.Header)
	upstreamHeader.Set("Authorization", "Bearer "+admission.Credential)
	upstreamClient := &http.Client{Transport: p.config.RuntimeCarrierTransport}
	upstream, response, err := websocket.Dial(dialCtx, upstreamURL, &websocket.DialOptions{
		HTTPClient:      upstreamClient,
		HTTPHeader:      upstreamHeader,
		Subprotocols:    []string{browserTerminalSubprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		refuse(terminalend.MachineUnavailable, err)
		return
	}
	if upstream.Subprotocol() != browserTerminalSubprotocol {
		upstream.CloseNow()
		refuse(terminalend.Protocol, ErrBrowserTerminalMessage)
		return
	}
	upstream.SetReadLimit(browserTerminalMaxRecordBytes)
	defer upstream.CloseNow()

	p.observeBrowserTerminal(ctx, bridgeBrowserTerminal(ctx, browser, upstream, subscription, func(cause error) (terminalend.Reason, bool) {
		return browserTerminalEnd(cause, admission)
	}))
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

// bridgeBrowserTerminal runs the browser <-> host bridge until one side ends it. endReason names the typed close
// the browser should get for the cause, if any; every other end drops the connection (the browser reconnects).
func bridgeBrowserTerminal(ctx context.Context, browser, upstream *websocket.Conn, subscription *BrowserTerminalSubscription, endReason func(error) (terminalend.Reason, bool)) (result error) {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	failures := make(chan error, 3)
	defer func() {
		// Close the browser first: cancelling the context under a blocked read hard-closes the connection and
		// the typed reason would never reach it.
		if endReason != nil {
			if reason, typed := endReason(result); typed {
				closeBrowserTerminal(browser, reason)
			}
		}
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
			// Wait for the writer: a browser that stops reading (a frozen or sleeping page) holds the host back
			// through TCP instead of ending the terminal.
			select {
			case outbound <- message:
			case <-ctx.Done():
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

// closeBrowserTerminal tells the browser why its terminal ended: the close carries the code and the typed reason.
func closeBrowserTerminal(browser *websocket.Conn, reason terminalend.Reason) {
	_ = browser.Close(websocket.StatusCode(reason.CloseCode()), string(reason))
}

// browserTerminalEnd names the typed close for how an attached terminal ended: the workspace's allowance cut it, or
// the control plane ended the admission and said why. Any other end (a dropped connection, the host going away) is a
// plain drop: the console reconnects and learns the reason from the next ticket.
func browserTerminalEnd(cause error, admission control.BrowserTerminalAdmission) (terminalend.Reason, bool) {
	if errors.Is(cause, ErrIngressQuotaExhausted) {
		return terminalend.BandwidthQuotaExhausted, true
	}
	if admission.EndReason != nil {
		if reason := admission.EndReason(); reason != "" {
			return reason, true
		}
	}
	return "", false
}

var errBrowserTerminalAdmissionIncomplete = errors.New("browser terminal admission is incomplete")

// browserTerminalRefusal is a refusal the edge sent the browser, reported with its reason.
type browserTerminalRefusal struct {
	reason terminalend.Reason
	cause  error
}

func (e browserTerminalRefusal) Error() string {
	return "browser terminal refused (" + string(e.reason) + "): " + e.cause.Error()
}
func (e browserTerminalRefusal) Unwrap() error { return e.cause }

// Expected shutdown must account for every leaf so a simultaneous operational
// failure is never hidden by a normal participant departure.
func browserTerminalExpected(err error) bool {
	return requestErrorLeaves(err, func(leaf error) bool {
		// The allowance cut is a plan limit the server already reported, not an edge failure.
		if leaf == context.Canceled || leaf == ErrBrowserTerminalHubEnded || leaf == ErrIngressQuotaExhausted {
			return true
		}
		// The control plane's own answers about the user's access are not edge failures; it reported them.
		if refused, ok := leaf.(*control.BrowserTerminalRefused); ok {
			switch refused.Reason {
			case terminalend.NoAccess, terminalend.Ended, terminalend.Revoked, terminalend.Expired, terminalend.Limit, terminalend.BandwidthQuotaExhausted, terminalend.TooManyPending:
				return true
			}
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

func browserTerminalUsage(ctx context.Context, environment string, admission control.BrowserTerminalAdmission, now time.Time) context.Context {
	if admission.AttachmentID == "" || environment == "" {
		return ctx
	}
	return withRuntimeUsage(ctx, runtimeUsageBinding{Authority: admission.AttachmentID, Environment: environment, IssuedAt: now})
}
