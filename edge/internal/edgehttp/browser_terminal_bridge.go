package edgehttp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/edge/internal/route"
	"github.com/pinksaucepasta/paperboat/shared/terminalend"
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
	// Until the bridge runs, an ended admission cancels the dial; the bridge then watches it itself, so
	// it closes the browser with the reason before anything cancels its reads (a canceled read
	// hard-closes the WebSocket, and the reason was lost).
	admissionWatcherDone := make(chan struct{})
	bridging := make(chan struct{})
	defer func() { cancel(); <-admissionWatcherDone }()
	go func() {
		defer close(admissionWatcherDone)
		select {
		case <-admission.Closed:
			cancel()
		case <-bridging:
		case <-ctx.Done():
		}
	}()

	// Typing and terminal output are not counted against the allowance and never slowed by it; only the workspace's terminal cap paces
	// them (terminalPacing). The edge cannot see inside the terminal's end-to-end encrypted stream, so the file transfers are the records
	// the browser marked (browserTerminalFileDiscriminator): those count toward the allowance under this attachment and are slowed by it
	// past the allowance. The plan limit (open browser terminals) is counted by the control plane.
	fileBytes := p.terminalFileUsage(ctx, match, admission)
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

	close(bridging)
	moved := false
	sent := 0 // the typed close code the browser got; 0 when the connection was dropped plain
	ended := bridgeBrowserTerminal(ctx, browser, upstream, subscription, admission.Closed, func(cause error) (terminalend.Reason, bool) {
		reason, typed := browserTerminalEnd(cause, admission)
		if !typed {
			// A terminal pb update's retiring generation held until it stopped, or this edge process handed over to
			// its replacement and ended the session: the new one serves already.
			moved = p.config.BrowserTerminalHub.Moved(key.RouteID, fence) || p.config.BrowserTerminalHub.MovedByHandOver(cause)
			reason, typed = terminalend.Moved, moved
		}
		if typed {
			sent = reason.CloseCode()
		}
		return reason, typed
	}, terminalPacing{throttles: p.config.Throttles, host: host, fileHost: "files:" + host, fileBytes: fileBytes})
	logBrowserTerminalEnd(ctx, ended, sent)
	if !moved { // the end of a drain is expected, not an edge failure
		p.observeBrowserTerminal(ctx, ended)
	}
}

// bridgeEnd names the side of the bridge whose read or write ended it.
type bridgeEnd struct {
	step string
	err  error
}

func (e bridgeEnd) Error() string { return e.step + ": " + e.err.Error() }
func (e bridgeEnd) Unwrap() error { return e.err }

// logBrowserTerminalEnd records how every browser terminal bridge ended, expected ends included: a drop the
// browser sees as connection_lost otherwise leaves no trace. Only fixed values are logged, never error text.
func logBrowserTerminalEnd(ctx context.Context, err error, sent int) {
	step := "unknown"
	var end bridgeEnd
	if errors.As(err, &end) {
		step = end.step
	}
	slog.InfoContext(ctx, "browser_terminal_ended", "step", step, "close_status", int(websocket.CloseStatus(err)),
		"canceled", errors.Is(err, context.Canceled), "hub_ended", errors.Is(err, ErrBrowserTerminalHubEnded), "close_sent", sent)
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

// bridgeEndCanceled reports whether a worker's error is only the request being canceled, not an edge failure. The
// workers run on ctx, which only the bridge cancels (after the typed close), so a cancel from outside shows on parent:
// the connections it ends fail with closed-connection errors before ctx is canceled. Both count.
func bridgeEndCanceled(parent, ctx context.Context, err error) bool {
	return requestCanceled(parent, err) || requestCanceled(ctx, err)
}

// bridgeBrowserTerminal runs the browser <-> host bridge until one side ends it or stop closes (the control plane
// ended the admission). endReason names the typed close the browser should get for the cause, if any; every other
// end drops the connection (the browser reconnects).
func bridgeBrowserTerminal(ctx context.Context, browser, upstream *websocket.Conn, subscription *BrowserTerminalSubscription, stop <-chan struct{}, endReason func(error) (terminalend.Reason, bool), pace terminalPacing) (result error) {
	// The reads and writes run on a context only this function cancels, after the typed close: a request canceled
	// from outside (the carrier it rode ended) would otherwise hard-close the browser's WebSocket under its blocked
	// read before the close below, and the browser got a plain drop instead of "moved".
	parent := ctx
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
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
		if bridgeEndCanceled(parent, ctx, err) {
			return
		}
		select {
		case failures <- err:
		default:
		}
	}

	toHost, _ := pace.start(ctx, &workers, func(err error) { report(bridgeEnd{"host_write", err}) }, func(payload []byte) error {
		return upstream.Write(ctx, websocket.MessageBinary, payload)
	})
	workers.Add(1)
	go func() {
		defer workers.Done()
		defer close(toHost)
		for {
			kind, message, err := browser.Read(ctx)
			if err != nil {
				report(bridgeEnd{"browser_read", err})
				return
			}
			if kind != websocket.MessageBinary {
				report(bridgeEnd{"browser_read", ErrBrowserTerminalMessage})
				return
			}
			payload, file, err := browserTerminalTLSMessagePayload(message)
			if err != nil {
				report(bridgeEnd{"browser_read", err})
				return
			}
			select {
			case toHost <- terminalFrame{payload: payload, file: file}:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Both directions of output (the host's stream and the shared output) are paced; the bridge's own writer takes the queue.
	forwardOutbound := func(message []byte) error {
		select {
		case outbound <- message:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	toBrowser, hostDone := pace.start(ctx, &workers, func(err error) { report(bridgeEnd{"browser_queue", err}) }, forwardOutbound)
	toBrowserShared, _ := pace.start(ctx, &workers, func(err error) { report(bridgeEnd{"browser_queue", err}) }, forwardOutbound)
	workers.Add(1)
	go func() {
		defer workers.Done()
		defer close(toBrowser)
		for {
			kind, payload, err := upstream.Read(ctx)
			if err != nil {
				report(bridgeEnd{"host_read", err})
				return
			}
			if kind != websocket.MessageBinary || len(payload) == 0 || len(payload) > browserTerminalMaxRecordBytes {
				report(bridgeEnd{"host_read", ErrBrowserTerminalMessage})
				return
			}
			message, err := browserTerminalTLSMessage(payload)
			if err != nil {
				report(bridgeEnd{"host_read", err})
				return
			}
			// Wait for the writer: a browser that stops reading (a frozen or sleeping page) holds the host back
			// through TCP instead of ending the terminal.
			select {
			case toBrowser <- terminalFrame{payload: message}:
			case <-ctx.Done():
				return
			}
		}
	}()

	workers.Add(1)
	go func() {
		defer workers.Done()
		defer close(toBrowserShared)
		for {
			record, err := subscription.Next(ctx)
			var message []byte
			if errors.Is(err, ErrBrowserTerminalLagged) {
				message = []byte{browserTerminalLaggedDiscriminator}
			} else if err != nil {
				report(bridgeEnd{"shared_output", err})
				return
			} else if message, err = browserTerminalSharedOutputMessagePayload(record); err != nil {
				report(bridgeEnd{"shared_output", err})
				return
			}
			// The hub owns the bounded per-browser backlog. Wait for the
			// writer here so an ordinary burst cannot fill the small bridge
			// multiplexing queue and force a fresh screen.
			select {
			case toBrowserShared <- terminalFrame{payload: message}:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case <-parent.Done():
			return parent.Err()
		case <-subscription.Done():
			return bridgeEnd{"shared_output", ErrBrowserTerminalHubEnded}
		case <-stop:
			flushHostFrames(ctx, browser, outbound, hostDone)
			return bridgeEnd{"admission_ended", context.Canceled}
		case cause := <-failures:
			flushHostFrames(ctx, browser, outbound, hostDone)
			return cause
		case message := <-outbound:
			if err := browser.Write(ctx, websocket.MessageBinary, message); err != nil {
				return bridgeEnd{"browser_write", err}
			}
		}
	}
}

// browserTerminalFlushBound bounds how long an ending bridge keeps forwarding what the host already sent.
const browserTerminalFlushBound = time.Second

// flushHostFrames forwards frames the host sent before the end (its terminal_stream_end event carries the exit
// status) ahead of the typed close: the host ends its stream right after that event and the control plane ends the
// admission when it sees the session gone, and either end used to drop frames still queued here. It waits for the
// host side to finish, at most browserTerminalFlushBound.
func flushHostFrames(ctx context.Context, browser *websocket.Conn, outbound <-chan []byte, hostDone <-chan struct{}) {
	ctx, cancel := context.WithTimeout(ctx, browserTerminalFlushBound)
	defer cancel()
	for {
		select {
		case message := <-outbound:
			if browser.Write(ctx, websocket.MessageBinary, message) != nil {
				return
			}
		case <-hostDone:
			for {
				select {
				case message := <-outbound:
					if browser.Write(ctx, websocket.MessageBinary, message) != nil {
						return
					}
				default:
					return
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

// closeBrowserTerminal tells the browser why its terminal ended: the close carries the code and the typed reason.
func closeBrowserTerminal(browser *websocket.Conn, reason terminalend.Reason) {
	_ = browser.Close(websocket.StatusCode(reason.CloseCode()), string(reason))
}

// browserTerminalEnd names the typed close for how an attached terminal ended: the control plane ended the admission and said why. Any other end (a dropped connection, the host going away) is a
// plain drop: the console reconnects and learns the reason from the next ticket.
func browserTerminalEnd(cause error, admission control.BrowserTerminalAdmission) (terminalend.Reason, bool) {
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
		if leaf == context.Canceled || leaf == ErrBrowserTerminalHubEnded {
			return true
		}
		// The control plane's own answers about the user's access are not edge failures; it reported them.
		if refused, ok := leaf.(*control.BrowserTerminalRefused); ok {
			switch refused.Reason {
			case terminalend.NoAccess, terminalend.Ended, terminalend.Revoked, terminalend.Expired, terminalend.Limit, terminalend.TooManyPending:
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

// terminalFileUsage returns what records n bytes of this terminal's file transfers under its attachment, the usage authority the server
// recorded when it admitted the ticket (nil when the edge has no meter or the attachment, environment or route is unknown).
func (p *Policy) terminalFileUsage(ctx context.Context, match route.RouteMatch, admission control.BrowserTerminalAdmission) func(int) {
	environment := match.Rule.Environment
	if environment == "" && p.config.HelperAccess != nil {
		// A runtime route's rule carries no environment; the server-signed host credential of this admission names the machine's one.
		if claims, err := p.config.HelperAccess.VerifyHelperAccess(ctx, admission.Credential); err == nil {
			environment = claims.EnvironmentID
		}
	}
	if p.config.RuntimeUsage == nil || admission.AttachmentID == "" || environment == "" {
		return nil
	}
	routeID, revision := match.Rule.RouteID, match.Rule.Revision
	return func(n int) {
		if err := p.config.RuntimeUsage.Record(environment, routeID, revision, uint64(n), 0, admission.AttachmentID); err != nil {
			slog.WarnContext(ctx, "browser_terminal_file_usage_failed", "route_id", routeID, "error", err)
		}
	}
}
