package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/config"
)

// A descriptor request the server refused with 503 (a serialization conflict under 40 concurrent execs,
// / support_45f4b28f) is asked again with backoff; other refusals are final, and repeats are
// bounded.
func TestDescriptorIssuanceRetriesServiceUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		statuses  []int
		wantCalls int32
		wantErr   bool
	}{
		{"recovers", []int{503, 429, 200}, 3, false},
		{"final refusal", []int{403}, 1, true},
		{"bounded", []int{503, 503, 503, 503, 503, 503, 503}, issuanceAttempts, true},
		{"lost answer", []int{0, 200}, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				status := tc.statuses[min(int(n), len(tc.statuses))-1]
				if status == 0 { // the request arrived, then the connection dropped: the answer is lost
					conn, _, _ := w.(http.Hijacker).Hijack()
					_ = conn.Close()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				if status != 200 {
					_, _ = w.Write([]byte(`{"error":{"code":"credential_unavailable","message":"x"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"data":{}}`))
			}))
			defer server.Close()
			client := New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
			var out map[string]any
			err := client.doIssuance(context.Background(), http.MethodPost, "/v1/machines/m/exec-descriptor", map[string]string{"operation_id": "operation_1"}, &out, false)
			var refused *APIError
			if calls.Load() != tc.wantCalls || (err != nil) != tc.wantErr || tc.wantErr && !errors.As(err, &refused) {
				t.Fatalf("calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

// F21-164: one open in 100 failed on a 503 from the create-and-connect request under a burst of opens. It is asked
// again with the same idempotency key, so a repeat resolves the same session.
func TestSessionCreateDescriptorRetriesServiceUnavailable(t *testing.T) {
	var calls atomic.Int32
	keys := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			CreateSession map[string]string `json:"create_session"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		keys[body.CreateSession["idempotency_key"]] = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"service_busy","message":"x"}}`))
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	client.SetSourceMachineID("machine_local")
	_, _, err := client.UserMachineConnectionDescriptorWithSessionCreate(context.Background(), "machine_target", "fresh", "create-key", "")
	if err == nil || calls.Load() != issuanceAttempts || len(keys) != 1 || !keys["create-key"] {
		t.Fatalf("calls=%d keys=%v err=%v", calls.Load(), keys, err)
	}
}

// F21-164: a session list answered 503 by a busy server is read again, not failed (14 of 91 cleanups failed on it).
func TestInventoryListRetriesServiceUnavailable(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"service_busy","message":"x"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"items":[{"id":"pts_1","name":"api","state":"running","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}],"pagination":{"limit":200,"offset":0,"total":1,"next_offset":null}}}`))
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	sessions, err := client.ListUserMachineTerminalSessions(context.Background(), "um_1")
	if err != nil || len(sessions) != 1 || calls.Load() != 3 {
		t.Fatalf("calls=%d sessions=%d err=%v", calls.Load(), len(sessions), err)
	}
}

// F21-164: a viewer joining an existing session asks for its connection again after a busy 503.
func TestSessionDescriptorRetriesServiceUnavailable(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"service_busy","message":"x"}}`))
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	client.SetSourceMachineID("machine_local")
	if _, err := client.UserMachineConnectionDescriptorForSession(context.Background(), "machine_target", "terminal_1"); err == nil || calls.Load() != issuanceAttempts {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
}
