package edgehttp

import (
	"context"
	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/control"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const browserConfigCompareSubprotocol = "paperboat.browser-config-compare.e2ee.v1"
const browserConfigComparePath = "/v1/browser-config-compare"

func (p *Policy) serveBrowserConfigCompare(w http.ResponseWriter, r *http.Request, host string, grant control.BrowserConfigCompareAdmission) {
	if grant.Close == nil || grant.Closed == nil || grant.Credential == "" || grant.AssignmentID == "" {
		if grant.Close != nil {
			grant.Close()
		}
		http.Error(w, "Comparison admission unavailable", 503)
		return
	}
	defer grant.Close()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-grant.Closed:
			cancel()
		case <-ctx.Done():
		}
	}()
	defer func() { cancel(); <-watcherDone }()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+grant.Credential)
	dialCtx, stop := context.WithTimeout(ctx, 10*time.Second)
	upstream, response, err := websocket.Dial(dialCtx, (&url.URL{Scheme: "wss", Host: host, Path: browserConfigComparePath}).String(), &websocket.DialOptions{HTTPClient: &http.Client{Transport: p.config.RuntimeCarrierTransport}, HTTPHeader: header, Subprotocols: []string{browserConfigCompareSubprotocol}, CompressionMode: websocket.CompressionDisabled})
	stop()
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		http.Error(w, "Comparison host unavailable", 502)
		return
	}
	defer upstream.CloseNow()
	if upstream.Subprotocol() != browserConfigCompareSubprotocol {
		http.Error(w, "Comparison protocol unavailable", 502)
		return
	}
	upstream.SetReadLimit(browserTerminalMaxRecordBytes)
	// Ticket admission binds the browser Origin; only opaque inner TLS records cross the edge.
	browser, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{browserConfigCompareSubprotocol}, InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer browser.CloseNow()
	browser.SetReadLimit(browserTerminalMaxRecordBytes + 1)
	_ = bridgeBrowserConfigCompare(ctx, browser, upstream)
}
func bridgeBrowserConfigCompare(ctx context.Context, browser, upstream *websocket.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	failures := make(chan error, 2)
	defer func() { cancel(); browser.CloseNow(); upstream.CloseNow(); workers.Wait() }()
	copyRecords := func(from, to *websocket.Conn, toBrowser bool) {
		defer workers.Done()
		for {
			kind, record, err := from.Read(ctx)
			if err != nil {
				failures <- err
				return
			}
			if kind != websocket.MessageBinary {
				failures <- ErrBrowserTerminalMessage
				return
			}
			if toBrowser {
				record, err = browserTerminalTLSMessage(record)
			} else {
				record, err = browserTerminalTLSMessagePayload(record)
			}
			if err != nil {
				failures <- err
				return
			}
			writeCtx, stop := context.WithTimeout(ctx, 10*time.Second)
			err = to.Write(writeCtx, websocket.MessageBinary, record)
			stop()
			if err != nil {
				failures <- err
				return
			}
		}
	}
	workers.Add(2)
	go copyRecords(browser, upstream, false)
	go copyRecords(upstream, browser, true)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-failures:
		return err
	}
}
