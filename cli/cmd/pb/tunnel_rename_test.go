package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

func TestTunnelRenameUsesCurrentResourceAndPreservesIdentity(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "rename", true: "stale write"}[stale], func(t *testing.T) {
			writes := 0
			withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
				current := validCommandTunnel()
				if r.URL.Path != "/v1/tunnels/tun_1" {
					t.Fatalf("unexpected resource: %s", r.URL.Path)
				}
				if r.Method == http.MethodGet {
					w.Header().Set("ETag", current.ETag)
					_ = json.NewEncoder(w).Encode(map[string]any{"data": current})
					return
				}
				writes++
				if r.Method != http.MethodPatch || r.Header.Get("If-Match") != current.ETag || r.Header.Get("Idempotency-Key") != "idem_test" {
					t.Fatalf("unsafe rename: %s %v", r.Method, r.Header)
				}
				var input map[string]any
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input) != 1 || input["name"] != "new-name" {
					t.Fatalf("rename body=%v error=%v", input, err)
				}
				if stale {
					w.WriteHeader(http.StatusPreconditionFailed)
					_, _ = w.Write([]byte(`{"error":{"code":"etag_mismatch","message":"refresh and retry"}}`))
					return
				}
				current.Name = "new-name"
				_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelMutation{
					Tunnel: current, Operation: validCommandOperation("tunnel", current.ID), Changed: true,
				}})
			})
			out, _, err := executeAdversarialTunnelCommand(t, "rename", "tun_1", "new-name", "--json")
			if writes != 1 {
				t.Fatalf("rename writes=%d error=%v", writes, err)
			}
			if stale {
				var remote *api.APIError
				if !errors.As(err, &remote) || remote.Code != "etag_mismatch" || out != "" {
					t.Fatalf("stale rename: output=%q error=%v", out, err)
				}
				return
			}
			var result api.TunnelMutation
			if err != nil || json.Unmarshal([]byte(out), &result) != nil || result.Tunnel.Name != "new-name" || result.Tunnel.ID != "tun_1" || result.Tunnel.StableEndpoint != validCommandTunnel().StableEndpoint {
				t.Fatalf("rename lost identity: output=%q error=%v", out, err)
			}
		})
	}
}
