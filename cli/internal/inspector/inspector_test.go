package inspector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
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

func TestRedactionPreservesDebuggability(t *testing.T) {
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
	if got := record.RequestHeaders.Get("Authorization"); got != "[redacted]" {
		t.Fatalf("authorization not redacted: %q", got)
	}
	if got := record.ResponseHeaders.Get("Set-Cookie"); got != "[redacted]" {
		t.Fatalf("set-cookie not redacted: %q", got)
	}
	if got := record.RequestHeaders.Get("Content-Type"); got != "application/json" {
		t.Fatalf("allowlisted content-type lost: %q", got)
	}
	if strings.Contains(record.URL, "session=secret") {
		t.Fatalf("query value leaked: %q", record.URL)
	}
	if !strings.Contains(record.URL, "/api/items") {
		t.Fatalf("path lost: %q", record.URL)
	}
	if strings.Contains(string(record.RequestBody), "hunter2") || strings.Contains(string(record.RequestBody), "abc") {
		t.Fatalf("body secret leaked: %s", record.RequestBody)
	}
	if !strings.Contains(string(record.RequestBody), "hello") {
		t.Fatalf("non-sensitive body lost: %s", record.RequestBody)
	}
	if strings.Contains(string(record.ResponseBody), "zzz") {
		t.Fatalf("response secret leaked: %s", record.ResponseBody)
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

func TestLongURLIsRedactedBeforeTruncation(t *testing.T) {
	secret := strings.Repeat("s", URLMaxBytes+100)
	got, truncated := SanitizeURL("https://example.test/path?token=" + secret)
	if !truncated {
		t.Fatal("long sanitized URL was not marked truncated")
	}
	if strings.Contains(got, secret[:128]) || strings.Contains(got, "token="+secret[:32]) {
		t.Fatalf("long URL leaked query value: %q", got)
	}
	if len(got) > URLMaxBytes {
		t.Fatalf("URL length = %d", len(got))
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
	policy.SensitiveNames = make([]string, MaxSensitiveNames+10)
	for i := range policy.SensitiveNames {
		policy.SensitiveNames[i] = "safe-name"
	}
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
	if len(store.policies["tunnel_replacement"].SensitiveNames) != MaxSensitiveNames {
		t.Fatal("sensitive-name policy was not bounded")
	}
}

func TestSanitizerBoundsAdversarialJSONAndURLInput(t *testing.T) {
	var document strings.Builder
	document.WriteByte('{')
	for i := 0; i < 2500; i++ {
		if i > 0 {
			document.WriteByte(',')
		}
		fmt.Fprintf(&document, `%q:%q`, fmt.Sprintf("field_%04d", i), "value")
	}
	document.WriteString(`,"password":"must-not-survive"}`)
	if document.Len() > BodyMaxBytes {
		t.Fatalf("fixture exceeded input cap: %d", document.Len())
	}
	sanitized, err := SanitizeJSONBody([]byte(document.String()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cap(sanitized) > BodyMaxBytes || strings.Contains(string(sanitized), "must-not-survive") || !json.Valid(sanitized) {
		t.Fatalf("adversarial JSON was not safely bounded/redacted: len=%d cap=%d", len(sanitized), cap(sanitized))
	}
	nested := strings.Repeat("[", JSONMaxDepth+2) + "0" + strings.Repeat("]", JSONMaxDepth+2)
	if _, err := SanitizeJSONBody([]byte(nested), nil); err != ErrInvalid {
		t.Fatalf("deep JSON = %v", err)
	}
	longURL := "https://example.test/" + strings.Repeat("path/", URLInputMaxBytes)
	if got, truncated := SanitizeURL(longURL); got != redactedValue || !truncated {
		t.Fatalf("oversized URL = %q, %v", got, truncated)
	}
}
