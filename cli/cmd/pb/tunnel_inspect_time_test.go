package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTunnelInspectUsesViewerTimezone(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("viewer", 5*3600+30*60)
	t.Cleanup(func() { time.Local = old })
	for _, mode := range []string{"list", "detail", "json"} {
		t.Run(mode, func(t *testing.T) {
			client, _ := newInspectorTestServer(t, http.StatusOK, inspectorTestRouteJSON)
			daemon := inspectorTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
				if mode == "detail" {
					var page struct {
						Records []json.RawMessage `json:"records"`
					}
					if err := json.Unmarshal([]byte(inspectorTestPage), &page); err != nil {
						t.Fatal(err)
					}
					_, _ = w.Write(page.Records[0])
				} else {
					_, _ = fmt.Fprint(w, inspectorTestPage)
				}
			})
			ctx := wireInspectorTestClients(daemon, client)
			args := []string{"inspect", "tun_01"}
			if mode == "detail" {
				args = append(args, "--id", "cap_01")
			}
			if mode == "json" {
				args = append(args, "--json")
			}
			output, err := runInspectorCommand(t, ctx, tunnelCobraCommandV1, args...)
			if err != nil {
				t.Fatal(err)
			}
			want := "2026-09-08T05:30:00+05:30"
			if mode == "json" {
				want = `"started_at":"2026-09-08T00:00:00Z"`
			}
			if !strings.Contains(output, want) {
				t.Fatalf("missing viewer time %q: %s", want, output)
			}
		})
	}
}
