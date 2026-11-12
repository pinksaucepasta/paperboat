package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestTunnelInspectMarksReplays(t *testing.T) {
	for _, mode := range []string{"list", "detail", "json"} {
		t.Run(mode, func(t *testing.T) {
			client, _ := newInspectorTestServer(t, http.StatusOK, inspectorTestRouteJSON)
			var page map[string]any
			if err := json.Unmarshal([]byte(inspectorTestPage), &page); err != nil {
				t.Fatal(err)
			}
			record := page["records"].([]any)[0].(map[string]any)
			record["replay_of"] = "cap_original"
			inspectorTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
				if mode == "detail" {
					_ = json.NewEncoder(w).Encode(record)
				} else {
					_ = json.NewEncoder(w).Encode(page)
				}
			})
			wireInspectorTestClients(client)
			args := []string{"inspect", "tun_01"}
			if mode == "detail" {
				args = append(args, "--id", "cap_01")
			}
			if mode == "json" {
				args = append(args, "--json")
			}
			output, err := runInspectorCommand(t, tunnelCobraCommandV1, args...)
			if err != nil {
				t.Fatal(err)
			}
			want := "replay of cap_original"
			if mode == "json" {
				want = `"replay_of":"cap_original"`
			}
			if !strings.Contains(output, want) {
				t.Fatalf("missing replay identity %q: %s", want, output)
			}
		})
	}
}
