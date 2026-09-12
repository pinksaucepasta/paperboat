package inspectorapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/inspector"
)

func TestReplayBindingGenerationAndExpiry(t *testing.T) {
	for _, field := range []string{"resource", "route", "target", "expiry"} {
		t.Run(field, func(t *testing.T) {
			now := time.Now().UTC()
			a := &fakeAuthorizer{decisions: map[string]Decision{"grant\x00tunnel\x00tun_01\x00rte_01\x00replay": testDecision("owner_01", "tunnel", "tun_01", "rte_01", now)}}
			s, store, registry := testService(t, a)
			id := seedCapture(t, store, "rte_01", now)
			calls := 0
			expiry := now.Add(2 * time.Second)
			binding := inspector.ReplayBinding{ResourceGeneration: 3, RouteGeneration: 3, TargetGeneration: 3, ExpiresAt: expiry, Forward: func(ctx context.Context, _ string, _ string, _ http.Header, _ []byte) (int, http.Header, []byte, bool, error) {
				calls++
				if deadline, ok := ctx.Deadline(); !ok || deadline.After(expiry) {
					t.Error("replay exceeds binding expiry")
				}
				return 200, nil, nil, false, nil
			}}
			switch field {
			case "resource":
				binding.ResourceGeneration--
			case "route":
				binding.RouteGeneration--
			case "target":
				binding.TargetGeneration--
			}
			_ = registry.Register("rte_01", binding)
			w := httptest.NewRecorder()
			body := replayHTTPRequest{ResourceKind: "tunnel", ResourceID: "tun_01", RouteID: "rte_01", CaptureID: id, IdempotencyKey: "fencing"}
			s.ServeHTTP(w, testRequest(t, http.MethodPost, "/v1/inspector/replay", body, "grant"))
			if field != "expiry" {
				if calls != 0 || w.Code != http.StatusConflict {
					t.Fatalf("stale binding dispatched: calls=%d status=%d", calls, w.Code)
				}
				return
			}
			if calls != 1 || w.Code != http.StatusOK {
				t.Fatalf("valid binding: calls=%d status=%d", calls, w.Code)
			}
			registry.Unregister("rte_01")
			retry := httptest.NewRecorder()
			s.ServeHTTP(retry, testRequest(t, http.MethodPost, "/v1/inspector/replay", body, "grant"))
			if calls != 1 || retry.Code != http.StatusOK || retry.Body.String() != w.Body.String() {
				t.Fatal("lost binding prevented recovery of recorded operation")
			}
		})
	}
}

func TestReplayIneligibilityReasonSurvivesHTTP(t *testing.T) {
	status, code, extra := inspectorStatus(&inspector.IneligibleError{Reason: "raw_truncated"})
	if status != http.StatusUnprocessableEntity || code != "inspector_replay_ineligible" || extra["reason"] != "raw_truncated" {
		t.Fatalf("lost typed reason: %d %s %v", status, code, extra)
	}
}
