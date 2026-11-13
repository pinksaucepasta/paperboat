package control

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/control/controltest"
	"github.com/pinksaucepasta/paperboat/shared/terminalend"
)

// A server rollout's proxy cuts the browser-terminal control channel 10 minutes after its reload
// (Caddy stream_close_delay: Close 1001, then both connections closed). The edge resumes the same
// attachment with its ticket and the admission stays open, with no control failure. A terminal
// revoked during the gap ends with the server's reason; a control outage longer than the resume
// grace still ends it, once, as a control failure.
func TestBrowserTerminalControlResumesAfterAProxyCut(t *testing.T) {
	for _, tc := range []struct {
		name    string
		during  func(*controltest.Plane)
		ended   bool
		reason  terminalend.Reason
		failure bool
	}{
		{name: "resumed"},
		{name: "revoked during the gap", during: func(p *controltest.Plane) { p.Refuse("revoked") }, ended: true, reason: terminalend.Revoked},
		{name: "outage longer than the grace", during: (*controltest.Plane).Down, ended: true, failure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := controlResumeGrace
			controlResumeGrace = 600 * time.Millisecond
			defer func() { controlResumeGrace = previous }()
			plane := controltest.Start(t)
			var failures atomic.Int32
			httpClient, err := NewHTTPClient(HTTPConfig{BaseURL: plane.URL, Credential: testControlCredential, Timeout: 3 * time.Second, Client: plane.Client,
				ControlFailure: func(context.Context, string, error) { failures.Add(1) }})
			if err != nil {
				t.Fatal(err)
			}
			client := &BrowserTerminalClient{HTTP: httpClient, NodeID: "edge_1", ProcessEpoch: "edge_epoch_1"}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			admission, err := client.Admit(ctx, "one-use", "https://dashboard.example.test", "machine.runtime.example.test")
			if err != nil {
				t.Fatal(err)
			}
			defer admission.Close()
			time.Sleep(3 * controltest.Heartbeat)
			if tc.during != nil {
				tc.during(plane)
			}
			cut := time.Now()
			plane.Cut()
			if !tc.ended {
				select {
				case <-admission.Closed:
					t.Fatalf("admission ended at a proxy cut (reason %q)", admission.EndReason())
				case <-time.After(controlResumeGrace + 10*controltest.Heartbeat):
				}
				requests := plane.Requests()
				if len(requests) != 2 || requests[1].Resume != "attach_1" || requests[1].Ticket != "one-use" {
					t.Fatalf("control requests %+v, want the admission then its resume", requests)
				}
			} else {
				select {
				case <-admission.Closed:
				case <-time.After(controlResumeGrace + 2*time.Second):
					t.Fatal("admission outlived a refused or failed resume")
				}
				if took := time.Since(cut); tc.failure && took < controlResumeGrace {
					t.Fatalf("outage ended the admission after %v, inside the resume grace", took)
				}
				if got := admission.EndReason(); got != tc.reason {
					t.Fatalf("end reason %q, want %q", got, tc.reason)
				}
			}
			if want := map[bool]int32{false: 0, true: 1}[tc.failure]; failures.Load() != want {
				t.Fatalf("control failures %d, want %d", failures.Load(), want)
			}
		})
	}
}
