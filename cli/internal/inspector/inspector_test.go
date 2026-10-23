package inspector

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func testCredential(resourceID, principal string, action Action, now time.Time) Credential {
	return Credential{
		PrincipalID:        principal,
		Action:             action,
		ResourceID:         resourceID,
		ResourceGeneration: 3,
		RouteGeneration:    3,
		TargetGeneration:   3,
		AuthorityReadAt:    now,
		ExpiresAt:          now.Add(time.Minute),
	}
}

func testPolicy() ResourcePolicy {
	return ResourcePolicy{
		Enabled:             true,
		CaptureRequestBody:  true,
		CaptureResponseBody: true,
		CaptureRaw:          true,
		ResourceGeneration:  3,
		RouteGeneration:     3,
		TargetGeneration:    3,
	}
}

func completeJSON(method, rawURL string, requestBody, responseBody []byte, raw []byte, now time.Time) CompletedCapture {
	requestHeaders := http.Header{"Content-Type": {"application/json"}, "Authorization": {"Bearer secret"}, "X-Paperboat-Internal": {"internal"}}
	responseHeaders := http.Header{"Content-Type": {"application/json"}, "Set-Cookie": {"session=secret"}}
	return CompletedCapture{
		Method:             method,
		RawURL:             rawURL,
		RequestHeaders:     requestHeaders,
		ResponseHeaders:    responseHeaders,
		RequestBody:        requestBody,
		ResponseBody:       responseBody,
		ResponseStatus:     200,
		ResourceGeneration: 3,
		RouteGeneration:    3,
		TargetGeneration:   3,
		StartedAt:          now,
		FinishedAt:         now.Add(20 * time.Millisecond),
		RawRequest:         raw,
	}
}

func TestDisabledByDefault(t *testing.T) {
	store := NewStore()
	if _, err := store.TryBegin("tunnel_01"); err != ErrDisabled {
		t.Fatalf("expected ErrDisabled, got %v", err)
	}
}

// Captures keep requests as sent, like ngrok: headers, query and body in full.
func TestCaptureKeepsHeadersQueryAndBodyAsSent(t *testing.T) {
	now := time.Now().UTC()
	store := NewStore()
	store.now = func() time.Time { return now }
	if err := store.SetPolicy("tunnel_01", testPolicy()); err != nil {
		t.Fatal(err)
	}
	pending, err := store.TryBegin("tunnel_01")
	if err != nil {
		t.Fatal(err)
	}
	requestBody := []byte(`{"message":"hello","password":"hunter2","nested":{"api_key":"abc"}}`)
	responseBody := []byte(`{"ok":true,"token":"zzz"}`)
	record, err := store.Finish(pending, completeJSON("POST", "https://app.example.test/api/items?session=secret&next=/x", requestBody, responseBody, []byte("POST /api/items HTTP/1.1\r\n\r\n{}"), now))
	if err != nil {
		t.Fatal(err)
	}
	// Headers are stored in full for people allowed to inspect.
	if got := record.RequestHeaders.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("authorization = %q, want it in full", got)
	}
	if got := record.RequestHeaders.Get("X-Paperboat-Internal"); got != "internal" {
		t.Fatalf("x-paperboat-internal = %q, want it in full", got)
	}
	if got := record.ResponseHeaders.Get("Set-Cookie"); got != "session=secret" {
		t.Fatalf("set-cookie = %q, want it in full", got)
	}
	if got := record.RequestHeaders.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type lost: %q", got)
	}
	if want := "https://app.example.test/api/items?session=secret&next=/x"; record.URL != want {
		t.Fatalf("url = %q, want %q", record.URL, want)
	}
	if string(record.RequestBody) != string(requestBody) {
		t.Fatalf("request body = %s, want it as sent", record.RequestBody)
	}
	if string(record.ResponseBody) != string(responseBody) {
		t.Fatalf("response body = %s, want it as sent", record.ResponseBody)
	}
	if record.RequestBodyState != StateComplete || record.ResponseBodyState != StateComplete {
		t.Fatalf("body states = %q/%q", record.RequestBodyState, record.ResponseBodyState)
	}
}

func TestBodyOptInShowsTruncationAndDrop(t *testing.T) {
	now := time.Now().UTC()
	store := NewStore()
	store.now = func() time.Time { return now }
	policy := testPolicy()
	policy.CaptureRequestBody = false
	if err := store.SetPolicy("tunnel_01", policy); err != nil {
		t.Fatal(err)
	}
	pending, err := store.TryBegin("tunnel_01")
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Finish(pending, completeJSON("POST", "https://app.example.test/x", []byte(`{"a":1}`), []byte(`{"ok":true}`), nil, now))
	if err != nil {
		t.Fatal(err)
	}
	if record.RequestBodyState != StateDropped || len(record.RequestBody) != 0 {
		t.Fatalf("disabled request body must drop without bytes: %+v", record.RequestBodyState)
	}
	if record.ResponseBodyState != StateComplete {
		t.Fatalf("enabled response body must complete: %+v", record.ResponseBodyState)
	}

	// Truncated bodies keep an explicit truncated state instead of being
	// presented as complete.
	big := make([]byte, BodyMaxBytes+16)
	for i := range big {
		big[i] = 'a'
	}
	withRequestBody := testPolicy()
	withRequestBody.CaptureRequestBody = true
	withRequestBody.CaptureResponseBody = true
	if err := store.SetPolicy("tunnel_01", withRequestBody); err != nil {
		t.Fatal(err)
	}
	pending, err = store.TryBegin("tunnel_01")
	if err != nil {
		t.Fatal(err)
	}
	completed := completeJSON("POST", "https://app.example.test/x", big, []byte(`{"ok":true}`), nil, now)
	completed.RequestTruncated = true
	record, err = store.Finish(pending, completed)
	if err != nil {
		t.Fatal(err)
	}
	if record.RequestBodyState != StateTruncated {
		t.Fatalf("over-limit body must be truncated: %+v", record.RequestBodyState)
	}
	if record.State != StateTruncated {
		t.Fatalf("record state must reflect truncation: %+v", record.State)
	}

	// Binary bodies are never presented as safely redacted.
	pending, err = store.TryBegin("tunnel_01")
	if err != nil {
		t.Fatal(err)
	}
	completed = completeJSON("POST", "https://app.example.test/x", []byte{0, 1, 2, 3}, []byte(`{"ok":true}`), nil, now)
	record, err = store.Finish(pending, completed)
	if err != nil {
		t.Fatal(err)
	}
	if record.RequestBodyState != StateUnsupported || len(record.RequestBody) != 0 {
		t.Fatalf("binary body must be unsupported without bytes: %+v", record.RequestBodyState)
	}
}

func TestViewerLacksInspectionAccess(t *testing.T) {
	now := time.Now().UTC()
	store := NewStore()
	store.now = func() time.Time { return now }
	if err := store.SetPolicy("tunnel_01", testPolicy()); err != nil {
		t.Fatal(err)
	}
	pending, err := store.TryBegin("tunnel_01")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finish(pending, completeJSON("GET", "https://app.example.test/", nil, nil, []byte("GET / HTTP/1.1\r\n\r\n"), now)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	viewer := testCredential("tunnel_01", "user_01", ActionView, now)
	if _, err := store.List(ctx, viewer, "", 10); err != ErrForbidden {
		t.Fatalf("viewer list must be forbidden, got %v", err)
	}
	if _, err := store.Get(ctx, viewer, "anything"); err != ErrForbidden {
		t.Fatalf("viewer get must be forbidden, got %v", err)
	}
	inspector := testCredential("tunnel_01", "user_01", ActionInspect, now)
	page, err := store.List(ctx, inspector, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 {
		t.Fatalf("inspect must list 1 record, got %d", len(page.Records))
	}
	// Raw bytes are never part of ordinary retrieval.
	if _, err := store.GetRaw(ctx, inspector, page.Records[0].ID); err != ErrForbidden {
		t.Fatalf("inspect credential must not open raw, got %v", err)
	}
	replayer := testCredential("tunnel_01", "user_01", ActionReplay, now)
	raw, err := store.GetRaw(ctx, replayer, page.Records[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "GET / HTTP/1.1\r\n\r\n" {
		t.Fatalf("raw mismatch: %q", raw)
	}
}

func TestStaleGenerationAndAuthorityDenied(t *testing.T) {
	now := time.Now().UTC()
	store := NewStore()
	store.now = func() time.Time { return now }
	if err := store.SetPolicy("tunnel_01", testPolicy()); err != nil {
		t.Fatal(err)
	}
	pending, err := store.TryBegin("tunnel_01")
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Finish(pending, completeJSON("GET", "https://app.example.test/", nil, nil, nil, now))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	stale := testCredential("tunnel_01", "user_01", ActionInspect, now)
	stale.ResourceGeneration = 2
	if _, err := store.Get(ctx, stale, record.ID); err != ErrStaleGeneration {
		t.Fatalf("stale generation must be denied, got %v", err)
	}
	expiredAuthority := testCredential("tunnel_01", "user_01", ActionInspect, now)
	expiredAuthority.AuthorityReadAt = now.Add(-time.Minute)
	if _, err := store.List(ctx, expiredAuthority, "", 10); err != ErrStaleAuthority {
		t.Fatalf("stale authority must be denied, got %v", err)
	}
}

func TestRetentionExpiryAndRevokePurge(t *testing.T) {
	now := time.Now().UTC()
	store := NewStore()
	current := now
	store.now = func() time.Time { return current }
	if err := store.SetPolicy("tunnel_01", testPolicy()); err != nil {
		t.Fatal(err)
	}
	pending, err := store.TryBegin("tunnel_01")
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Finish(pending, completeJSON("GET", "https://app.example.test/", nil, nil, []byte("GET / HTTP/1.1\r\n\r\n"), now))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Raw expires after 2 minutes while sanitized metadata remains.
	current = now.Add(3 * time.Minute)
	credential := testCredential("tunnel_01", "user_01", ActionInspect, current)
	credential.AuthorityReadAt = current
	credential.ExpiresAt = current.Add(time.Minute)
	got, err := store.Get(ctx, credential, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReplayIneligible == "" {
		t.Fatal("raw expiry must mark replay ineligible")
	}
	replayer := credential
	replayer.Action = ActionReplay
	if _, err := store.GetRaw(ctx, replayer, record.ID); err != ErrExpired {
		t.Fatalf("expired raw must be denied, got %v", err)
	}
	// Full retention expiry purges the record.
	current = now.Add(Retention + time.Minute)
	credential.AuthorityReadAt = current
	credential.ExpiresAt = current.Add(time.Minute)
	if _, err := store.Get(ctx, credential, record.ID); err != ErrNotFound {
		t.Fatalf("expired record must be purged, got %v", err)
	}

	// Revocation purges immediately and denies retrieval.
	current = now
	store.now = func() time.Time { return current }
	pending, err = store.TryBegin("tunnel_01")
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.Finish(pending, completeJSON("GET", "https://app.example.test/", nil, nil, nil, now))
	if err != nil {
		t.Fatal(err)
	}
	store.Revoke("tunnel_01")
	credential = testCredential("tunnel_01", "user_01", ActionInspect, current)
	if _, err := store.Get(ctx, credential, record.ID); err == nil {
		t.Fatal("revoked resource must deny retrieval")
	}
}

func TestPerResourceBudgetIsolation(t *testing.T) {
	now := time.Now().UTC()
	store := NewStore()
	store.now = func() time.Time { return now }
	if err := store.SetPolicy("tunnel_01", testPolicy()); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPolicy("tunnel_02", testPolicy()); err != nil {
		t.Fatal(err)
	}
	// Fill tunnel_01 to its record bound; tunnel_02 must stay usable.
	inserted := 0
	for i := 0; i < ResourceMaxRecords+10; i++ {
		pending, err := store.TryBegin("tunnel_01")
		if err != nil {
			break
		}
		completed := completeJSON("GET", "https://app.example.test/", nil, nil, nil, now.Add(time.Duration(i)*time.Millisecond))
		if _, err := store.Finish(pending, completed); err != nil {
			break
		}
		inserted++
	}
	if inserted != ResourceMaxRecords+10 {
		t.Fatalf("completed ring froze instead of evicting: inserted %d", inserted)
	}
	pending, err := store.TryBegin("tunnel_02")
	if err != nil {
		t.Fatalf("second resource must stay usable: %v", err)
	}
	store.Abandon(pending)
	stats := store.Stats()
	if stats.DaemonRecords != ResourceMaxRecords {
		t.Fatalf("daemon records = %d", stats.DaemonRecords)
	}
}

func TestLongURLIsTruncatedOnARuneBoundary(t *testing.T) {
	long := "https://example.test/path?q=" + strings.Repeat("é", URLMaxBytes)
	got, truncated := SanitizeURL(long)
	if !truncated || len(got) > URLMaxBytes || !utf8.ValidString(got) || !strings.HasPrefix(long, got) {
		t.Fatalf("url = %d bytes, truncated=%v, valid=%v", len(got), truncated, utf8.ValidString(got))
	}
	if got, truncated := SanitizeURL("https://example.test/" + strings.Repeat("path/", URLInputMaxBytes)); got != truncatedURL || !truncated {
		t.Fatalf("oversized URL = %q, %v", got, truncated)
	}
}

// Text bodies of any content type are stored as sent; binary ones are marked unsupported.
func TestBodiesAreStoredAsTextOrMarkedUnsupported(t *testing.T) {
	if body, state := sanitizeBody([]byte("a=1&password=x"), false, false, true); string(body) != "a=1&password=x" || state != StateComplete {
		t.Fatalf("form body = %q %q", body, state)
	}
	if body, state := sanitizeBody([]byte{0x89, 'P', 'N', 'G', 0}, false, false, true); body != nil || state != StateUnsupported {
		t.Fatalf("binary body = %q %q", body, state)
	}
	long := []byte(strings.Repeat("é", BodyMaxBytes))
	body, state := sanitizeBody(long[:BodyMaxBytes], true, false, true)
	if state != StateTruncated || !utf8.Valid(body) || len(body) == 0 || len(body) > BodyMaxBytes {
		t.Fatalf("truncated body = %d bytes %q", len(body), state)
	}
}

func TestPendingTapsHonorPolicyAndRawBound(t *testing.T) {
	store := NewStore()
	policy := testPolicy()
	policy.CaptureRequestBody = false
	if err := store.SetPolicy("tunnel_01", policy); err != nil {
		t.Fatal(err)
	}
	pending, err := store.TryBegin("tunnel_01")
	if err != nil {
		t.Fatal(err)
	}
	requestTap := pending.RequestBodyTap()
	_, _ = requestTap.Write([]byte("must-not-retain"))
	if got := requestTap.Bytes(); len(got) != 0 {
		t.Fatalf("disabled request tap retained %q", got)
	}
	prefix := []byte("POST / HTTP/1.1\r\nHost: example.test\r\n\r\n")
	rawTap := pending.RawRequestTap(prefix)
	_, _ = rawTap.Write(make([]byte, RawMaxBytes))
	raw, truncated := rawTap.Take()
	if !truncated || len(raw) != RawMaxBytes || !strings.HasPrefix(string(raw), string(prefix)) {
		t.Fatalf("raw tap len/truncated/prefix = %d/%v/%v", len(raw), truncated, strings.HasPrefix(string(raw), string(prefix)))
	}
	if cap(raw) > RawMaxBytes {
		t.Fatalf("raw tap capacity exceeded bound: %d", cap(raw))
	}
	_, _ = rawTap.Write([]byte("late"))
	if len(rawTap.Bytes()) != 0 {
		t.Fatal("taken tap accepted late bytes")
	}
	store.Abandon(pending)
}

func TestInflightExpiryAndLateFinishAreFenced(t *testing.T) {
	now := time.Now().UTC()
	store := NewStore()
	store.now = func() time.Time { return now }
	policy := testPolicy()
	if err := store.SetPolicy("tunnel_01", policy); err != nil {
		t.Fatal(err)
	}
	old, err := store.TryBegin("tunnel_01")
	if err != nil {
		t.Fatal(err)
	}
	tap := old.RawRequestTap([]byte("GET / HTTP/1.1\r\n\r\n"))
	if store.Stats().DaemonRecords != 1 || store.Stats().DaemonBytes == 0 {
		t.Fatal("active reservation not accounted")
	}
	now = now.Add(RawRetention)
	_ = store.Stats()
	_, _ = tap.Write([]byte("late"))
	if len(tap.Bytes()) != 0 {
		t.Fatal("raw-expired active tap retained late bytes")
	}
	now = now.Add(Retention - RawRetention)
	if store.Stats().DaemonRecords != 0 {
		t.Fatal("expired active reservation retained")
	}
	if len(tap.Bytes()) != 0 {
		t.Fatal("expired active buffer retained")
	}
	store.Revoke("tunnel_01")
	policy.Revoked = false
	if err := store.SetPolicy("tunnel_01", policy); err != nil {
		t.Fatal(err)
	}
	fresh, err := store.TryBegin("tunnel_01")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finish(old, completeJSON("GET", "https://example.test", nil, nil, nil, now)); err != ErrDisabled {
		t.Fatalf("late finish = %v", err)
	}
	if store.Stats().DaemonRecords != 1 {
		t.Fatal("late finish released fresh reservation")
	}
	store.Abandon(fresh)
}

func TestRunExpiresIdlePendingAtDeadlineAndPurgesOnCancel(t *testing.T) {
	store := NewStore()
	policy := testPolicy()
	policy.CaptureRaw = false
	if err := store.SetPolicy("tunnel_01", policy); err != nil {
		t.Fatal(err)
	}
	pending, err := store.TryBegin("tunnel_01")
	if err != nil {
		t.Fatal(err)
	}
	pending.StartedAt = time.Now().UTC().Add(-Retention + 30*time.Millisecond)
	tap := pending.RequestBodyTap()
	_, _ = tap.Write([]byte("retained"))
	store.mu.Lock()
	store.signalCleanupLocked()
	store.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		store.Run(ctx)
		close(done)
	}()
	time.Sleep(80 * time.Millisecond)
	store.mu.Lock()
	_, retained := store.pending[pending.ID]
	store.mu.Unlock()
	if retained || len(tap.Bytes()) != 0 {
		t.Fatal("idle deadline did not release pending capture")
	}

	fresh, err := store.TryBegin("tunnel_01")
	if err != nil {
		t.Fatal(err)
	}
	freshTap := fresh.RequestBodyTap()
	_, _ = freshTap.Write([]byte("purge-me"))
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop")
	}
	if len(freshTap.Bytes()) != 0 {
		t.Fatal("shutdown retained active buffer")
	}
	if stats := store.Stats(); stats.DaemonRecords != 0 || stats.DaemonBytes != 0 {
		t.Fatalf("shutdown retained state: %+v", stats)
	}
}

func TestPolicyAndUsageChurnRemainBounded(t *testing.T) {
	store := NewStore()
	policy := testPolicy()
	for i := 0; i < DaemonMaxRecords; i++ {
		id := fmt.Sprintf("tunnel_%04d", i)
		if err := store.SetPolicy(id, policy); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetPolicy("tunnel_overflow", policy); err != ErrDropped {
		t.Fatalf("policy overflow = %v", err)
	}
	store.Revoke("tunnel_0000")
	if err := store.SetPolicy("tunnel_replacement", policy); err != nil {
		t.Fatal(err)
	}
	if len(store.policies) != DaemonMaxRecords {
		t.Fatalf("policy entries = %d", len(store.policies))
	}
}

// A resource with no policy (capture never enabled, or purged) is "not enabled", not a stale credential.
func TestRetrievalWithoutAPolicyReportsCaptureNotEnabled(t *testing.T) {
	now := time.Now().UTC()
	store := NewStore()
	store.now = func() time.Time { return now }
	credential := testCredential("tunnel_01", "user_01", ActionInspect, now)
	if _, err := store.List(context.Background(), credential, "", 10); err != ErrDisabled {
		t.Fatalf("never enabled: %v", err)
	}
	if err := store.SetPolicy("tunnel_01", testPolicy()); err != nil {
		t.Fatal(err)
	}
	store.Revoke("tunnel_01")
	if _, err := store.List(context.Background(), credential, "", 10); err != ErrDisabled {
		t.Fatalf("after purge: %v", err)
	}
}

// A request that started first but finished after a later one is still listed after the cursor
// a reader holds: pages follow the order records were stored, not their start time.
func TestListCursorKeepsARecordThatFinishedLate(t *testing.T) {
	now := time.Now().UTC()
	store := NewStore()
	store.now = func() time.Time { return now }
	if err := store.SetPolicy("tunnel_01", testPolicy()); err != nil {
		t.Fatal(err)
	}
	slow, err := store.TryBegin("tunnel_01")
	if err != nil {
		t.Fatal(err)
	}
	fast, err := store.TryBegin("tunnel_01")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finish(fast, completeJSON("GET", "https://app.example.test/fast", nil, nil, nil, now.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	credential := testCredential("tunnel_01", "user_01", ActionInspect, now)
	first, err := store.List(context.Background(), credential, "", 10)
	if err != nil || len(first.Records) != 1 {
		t.Fatalf("first page = %+v err=%v", first.Records, err)
	}
	if _, err := store.Finish(slow, completeJSON("GET", "https://app.example.test/slow", nil, nil, nil, now)); err != nil {
		t.Fatal(err)
	}
	next, err := store.List(context.Background(), credential, first.Records[0].ID, 10)
	if err != nil || len(next.Records) != 1 || next.Records[0].URL != "https://app.example.test/slow" {
		t.Fatalf("page after cursor = %+v err=%v, want the late /slow record", next.Records, err)
	}
}
