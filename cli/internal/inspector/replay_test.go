package inspector

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func replayTestSetup(t *testing.T, policy ResourcePolicy, configured bool) (*Store, *Manager, string) {
	t.Helper()
	now := time.Now().UTC()
	store := NewStore()
	store.now = func() time.Time { return now }
	if !configured {
		policy = testPolicy()
	}
	if err := store.SetPolicy("tunnel_01", policy); err != nil {
		t.Fatal(err)
	}
	pending, err := store.TryBegin("tunnel_01")
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("POST /api/items?session=secret HTTP/1.1\r\nHost: origin.example\r\nContent-Type: application/json\r\nAuthorization: Bearer secret\r\nContent-Length: 15\r\n\r\n{\"message\":\"hi\"}")
	record, err := store.Finish(pending, completeJSON("POST", "https://app.example.test/api/items?session=secret", []byte(`{"message":"hi"}`), []byte(`{"ok":true}`), raw, now))
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store)
	if manager == nil {
		t.Fatal("nil manager")
	}
	return store, manager, record.ID
}

func replayTestRequest(id string, now time.Time) ReplayRequest {
	return ReplayRequest{
		PrincipalID:        "user_01",
		ResourceID:         "tunnel_01",
		CaptureID:          id,
		ResourceGeneration: 3,
		RouteGeneration:    3,
		TargetGeneration:   3,
		IdempotencyKey:     "replay_key_01",
		AuthorityReadAt:    now,
		ExpiresAt:          now.Add(time.Minute),
	}
}

func TestReplayPreservesBytesAndAudits(t *testing.T) {
	now := time.Now().UTC()
	_, manager, id := replayTestSetup(t, ResourcePolicy{}, false)
	var calls int32
	forward := func(ctx context.Context, method, requestURI string, header http.Header, body []byte) (int, http.Header, []byte, bool, error) {
		atomic.AddInt32(&calls, 1)
		if method != "POST" || requestURI != "/api/items?session=secret" {
			t.Errorf("replay target = %s %s", method, requestURI)
		}
		// Reserved credentials must be stripped; application auth is reused.
		if header.Get("X-Paperboat-Internal") != "" || header.Get("Content-Length") != "" {
			t.Errorf("framing/reserved headers leaked: %v", header)
		}
		if got := header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("application authorization lost: %q", got)
		}
		if string(body) != `{"message":"hi"}` {
			t.Errorf("body altered: %q", body)
		}
		return 200, http.Header{"Content-Type": {"application/json"}}, []byte(`{"ok":true}`), false, nil
	}
	result, err := manager.Replay(context.Background(), replayTestRequest(id, now), forward)
	if err != nil {
		t.Fatal(err)
	}
	if result.Method != "POST" || result.ResponseStatus != 200 || result.CaptureID != id || result.OperationID == "" {
		t.Fatalf("result = %+v", result)
	}
	if result.SideEffects == "" || result.ReplayRecordID == "" {
		t.Fatalf("result must show side effects and separate response record: %+v", result)
	}
	if calls != 1 {
		t.Fatalf("origin calls = %d, want 1", calls)
	}
	audit := manager.Audit()
	if len(audit) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(audit))
	}
	entry := audit[0]
	if entry.Actor != "user_01" || entry.Action != "replay" || entry.CaptureID != id || entry.OperationID != result.OperationID || entry.Outcome != "succeeded" {
		t.Fatalf("audit = %+v", entry)
	}
}

func TestReplayIdempotencyPreventsSecondOriginRequest(t *testing.T) {
	now := time.Now().UTC()
	_, manager, id := replayTestSetup(t, ResourcePolicy{}, false)
	var calls int32
	forward := func(context.Context, string, string, http.Header, []byte) (int, http.Header, []byte, bool, error) {
		atomic.AddInt32(&calls, 1)
		return 200, http.Header{"Content-Type": {"application/json"}}, []byte(`{"ok":true}`), false, nil
	}
	request := replayTestRequest(id, now)
	first, err := manager.Replay(context.Background(), request, forward)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Replay(context.Background(), request, forward)
	if err != nil || second.OperationID != first.OperationID || calls != 1 {
		t.Fatalf("duplicate = %+v err=%v calls=%d", second, err, calls)
	}
	conflict := request
	conflict.CaptureID = "other"
	if _, err := manager.Replay(context.Background(), conflict, forward); !errors.Is(err, ErrReplayConflict) {
		t.Fatalf("conflicting key must conflict, got %v", err)
	}
}

func TestReplayDeniesViewerStaleAndIncomplete(t *testing.T) {
	now := time.Now().UTC()
	store, manager, id := replayTestSetup(t, ResourcePolicy{}, false)
	forward := func(context.Context, string, string, http.Header, []byte) (int, http.Header, []byte, bool, error) {
		return 200, nil, nil, false, nil
	}
	_ = store
	// A viewer grant (inspect action) cannot open raw: Replay requires replay
	// credentials, and direct raw access with inspect stays forbidden.
	viewer := testCredential("tunnel_01", "user_01", ActionInspect, now)
	if _, err := store.GetRaw(context.Background(), viewer, id); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer raw = %v, want forbidden", err)
	}
	// Stale generations and stale authority are honest typed failures.
	stale := replayTestRequest(id, now)
	stale.ResourceGeneration = 2
	if _, err := manager.Replay(context.Background(), stale, forward); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("stale generation = %v", err)
	}
	expired := replayTestRequest(id, now)
	expired.AuthorityReadAt = now.Add(-time.Minute)
	if _, err := manager.Replay(context.Background(), expired, forward); !errors.Is(err, ErrStaleAuthority) {
		t.Fatalf("stale authority = %v", err)
	}
	// Truncated captures are honestly ineligible, never silently replayed.
	policy := testPolicy()
	if err := store.SetPolicy("tunnel_02", policy); err != nil {
		t.Fatal(err)
	}
	pending, _ := store.TryBegin("tunnel_02")
	big := make([]byte, BodyMaxBytes+8)
	for i := range big {
		big[i] = 'a'
	}
	completed := completeJSON("POST", "https://app.example.test/x", big, []byte(`{"ok":true}`), []byte("POST /x HTTP/1.1\r\n\r\n{}"), now)
	completed.RequestTruncated = true
	completed.RawTruncated = true
	truncated, err := store.Finish(pending, completed)
	if err != nil {
		t.Fatal(err)
	}
	truncatedReq := replayTestRequest(truncated.ID, now)
	truncatedReq.ResourceID = "tunnel_02"
	if _, err := manager.Replay(context.Background(), truncatedReq, forward); !errors.Is(err, ErrReplayIneligible) {
		t.Fatalf("truncated replay = %v, want ineligible", err)
	}
	// Missing raw (body capture without raw opt-in) is ineligible.
	noRawPolicy := testPolicy()
	noRawPolicy.CaptureRaw = false
	if err := store.SetPolicy("tunnel_03", noRawPolicy); err != nil {
		t.Fatal(err)
	}
	pending, _ = store.TryBegin("tunnel_03")
	plain, err := store.Finish(pending, completeJSON("GET", "https://app.example.test/", nil, nil, nil, now))
	if err != nil {
		t.Fatal(err)
	}
	noRawReq := replayTestRequest(plain.ID, now)
	noRawReq.ResourceID = "tunnel_03"
	if _, err := manager.Replay(context.Background(), noRawReq, forward); !errors.Is(err, ErrReplayIneligible) {
		t.Fatalf("raw-missing replay = %v, want ineligible", err)
	}
}

func TestReplaySurfacesOriginRejectionWithoutResigning(t *testing.T) {
	now := time.Now().UTC()
	_, manager, id := replayTestSetup(t, ResourcePolicy{}, false)
	forward := func(context.Context, string, string, http.Header, []byte) (int, http.Header, []byte, bool, error) {
		return 401, http.Header{"Content-Type": {"text/plain"}}, []byte("expired signature"), false, nil
	}
	result, err := manager.Replay(context.Background(), replayTestRequest(id, now), forward)
	if err != nil || result.ResponseStatus != 401 {
		t.Fatalf("expired signature must surface as origin result: %+v err=%v", result, err)
	}
}

func TestReplayConcurrencyAndAuditBounds(t *testing.T) {
	now := time.Now().UTC()
	_, manager, id := replayTestSetup(t, ResourcePolicy{}, false)
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	var concurrent int32
	var peak int32
	forward := func(ctx context.Context, _, _ string, _ http.Header, _ []byte) (int, http.Header, []byte, bool, error) {
		current := atomic.AddInt32(&concurrent, 1)
		for {
			observed := atomic.LoadInt32(&peak)
			if current <= observed || atomic.CompareAndSwapInt32(&peak, observed, current) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		atomic.AddInt32(&concurrent, -1)
		return 200, nil, nil, false, nil
	}
	// Daemon allows 4 concurrent replays across resources; per-resource allows 1.
	firstErr := make(chan error, 1)
	go func() {
		request := replayTestRequest(id, now)
		_, err := manager.Replay(context.Background(), request, forward)
		firstErr <- err
	}()
	<-started
	second := replayTestRequest(id, now)
	second.IdempotencyKey = "replay_key_02"
	done := make(chan error, 1)
	go func() { _, err := manager.Replay(context.Background(), second, forward); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrTooManyReads) {
			t.Fatalf("second concurrent replay on same resource = %v, want too-many", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("per-resource concurrency was not enforced")
	}
	close(release)
	if err := <-firstErr; err != nil {
		t.Fatal(err)
	}
}
