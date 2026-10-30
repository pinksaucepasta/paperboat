package tunnel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
)

type failedStalledConn struct{ stalledReconnectConn }

func (c failedStalledConn) Wait() (int, error) {
	<-c.stop
	return 0, errors.Join(ErrTransportLost, net.ErrClosed)
}

// The two real recovery owners may overlap. Evidence must identify both callers,
// their shared old link, each attempt's outcome and the original typed failure.
func TestReconnectDiagnosticsCorrelateOverlappingRecovery(t *testing.T) {
	recorder := diagnostics.NewMemoryRecorder()
	ctx, cancel := context.WithTimeout(diagnostics.WithRecorder(t.Context(), recorder), 5*time.Second)
	defer cancel()
	first := failedStalledConn{stalledReconnectConn{&blockingReconnectConn{reconnectTestConn: &reconnectTestConn{}, stop: make(chan struct{})}}}
	entered := make(chan string, 2)
	release := make(chan struct{})
	probeDone := make(chan struct{})
	c := NewReconnectingConn(ctx, first, 1, 0, func(ctx context.Context) (Conn, error) {
		h := http.Header{}
		diagnostics.ReconnectHeaders(ctx, h)
		parts := strings.Split(h.Get(diagnostics.ReconnectHeader), ":")
		if len(parts) != 3 {
			return nil, errors.New("missing reconnect context")
		}
		entered <- parts[1]
		end := diagnostics.ReconnectStep(ctx, "grant_fetch")
		<-release
		if parts[1] == "probeStalled" {
			end(context.Canceled)
			defer close(probeDone)
			return nil, context.Canceled
		}
		end(nil)
		return &reconnectTestConn{Reader: bytes.NewReader(nil)}, nil
	})
	defer c.Close()
	select {
	case caller := <-entered:
		if caller != "probeStalled" {
			t.Fatalf("first caller %s", caller)
		}
	case <-ctx.Done():
		t.Fatal("probe did not enter")
	}
	c.Redial()
	select {
	case caller := <-entered:
		if caller != "supervise" {
			t.Fatalf("second caller %s", caller)
		}
	case <-ctx.Done():
		t.Fatal("supervisor did not enter")
	}
	close(release)
	if _, err := io.ReadAll(c); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	<-probeDone
	// The callback's return precedes the final diagnostic; wait for that record, bounded by ctx.
	for {
		found := false
		for _, e := range recorder.Recent() {
			if e.Code == "redial" && e.Fields["caller"] == "probeStalled" && e.Fields["state"] == "failed" {
				found = true
			}
		}
		if found {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("missing probe outcome")
		case <-time.After(time.Millisecond):
		}
	}
	attempts := map[string]string{}
	ends := map[string]string{}
	connection := ""
	oldFailure := false
	steps := map[string]int{}
	for _, e := range recorder.Recent() {
		if e.Validate() != nil {
			t.Fatal("invalid diagnostic")
		}
		if e.Code == "old_connection" && e.Fields["state"] == "wait_returned" && e.Fields["cause"] == "connection_closed" && e.Fields["error_type"] != "" {
			oldFailure = true
		}
		if e.Code == "redial" {
			f := e.Fields
			if connection == "" {
				connection = f["connection_id"]
			}
			if connection != f["connection_id"] {
				t.Fatal("attempts lost old-link identity")
			}
			if f["state"] == "started" {
				attempts[f["attempt_id"]] = f["caller"]
			} else {
				ends[f["attempt_id"]] = f["state"]
			}
		}
		if e.Code == "redial_step" && e.Fields["step"] == "grant_fetch" {
			steps[e.Fields["step_id"]]++
			if e.Fields["attempt_id"] == "" || e.Fields["source_line"] == "" {
				t.Fatal("uncorrelated step")
			}
		}
	}
	if !oldFailure || len(attempts) != 2 || len(steps) != 2 {
		t.Fatalf("failure=%v attempts=%v steps=%v", oldFailure, attempts, steps)
	}
	for id, caller := range attempts {
		want := "ready"
		if caller == "probeStalled" {
			want = "failed"
		}
		if ends[id] != want {
			t.Fatalf("caller=%s outcome=%s", caller, ends[id])
		}
	}
	for _, count := range steps {
		if count != 2 {
			t.Fatal("unpaired step")
		}
	}
}
