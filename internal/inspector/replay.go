package inspector

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Replay bounds from the preview-tunnel-v1 contract. Replay is a deliberate
// user action: one new origin request to the same target, never an automatic
// transport retry and never a redirect to an arbitrary destination.
const (
	ReplayMaxConcurrentPerResource = 1
	ReplayMaxConcurrentDaemon      = 4
	ReplayTimeout                  = 30 * time.Second
	// ReplayAuditMaxEntries bounds safe audit/outcome metadata. Entries expire
	// after Retention; when the bound is reached after expiry, replay is
	// rejected instead of dropping audit history. Replays are deliberate and
	// infrequent, so 512 entries is ample without unbounded growth.
	ReplayAuditMaxEntries = 512
	// ReplayOperationIDPrefix marks derived request IDs so a replay is never
	// reported as the uninterrupted original exchange.
	ReplayOperationIDPrefix = "replay_"
)

var (
	ErrReplayIneligible = errors.New("inspector capture cannot be replayed")
	ErrReplayConflict   = errors.New("inspector replay idempotency conflict")
	ErrNoAuditCapacity  = errors.New("inspector audit capacity unavailable")
	ErrAmbiguousReplay  = errors.New("inspector replay result is ambiguous")
)

// IneligibleError carries the typed replay-ineligibility reason. Callers
// branch on Reason, never on message text.
type IneligibleError struct {
	Reason string
}

func (e *IneligibleError) Error() string { return "inspector capture cannot be replayed" }
func (e *IneligibleError) Is(target error) bool {
	return target == ErrReplayIneligible
}

// ReplayRequest is the only replay input. There is no destination, header or
// body override: the daemon replays the retained exact bytes to the same
// current target through its own target-bound transport.
type ReplayRequest struct {
	PrincipalID        string
	ResourceID         string
	CaptureID          string
	ResourceGeneration uint64
	RouteGeneration    uint64
	TargetGeneration   uint64
	IdempotencyKey     string
	AuthorityReadAt    time.Time
	ExpiresAt          time.Time
}

// ReplayForwarder sends one parsed replay request to the same current target.
// The inspector supplies method, origin-form request target, application
// headers (hop-by-hop and reserved Paperboat credentials already removed) and
// the exact retained body. The implementation must dial only the current
// bound target, enforce its TLS policy, and recompute transport framing. It
// returns the origin status/headers and a bounded response body prefix with a
// truncation flag. Transport failures return a non-nil error with no status.
type ReplayForwarder func(ctx context.Context, method, requestURI string, header http.Header, body []byte) (status int, responseHeader http.Header, responseBody []byte, truncated bool, err error)

// ReplayResult is the deliberate-action outcome shown in CLI/dashboard. It
// carries sanitized display fields only; raw bytes are never included.
type ReplayResult struct {
	OperationID       string
	CaptureID         string
	ResourceID        string
	Method            string
	URL               string
	RequestBodyState  CaptureState
	ResponseStatus    int
	ResponseBodyState CaptureState
	// ReplayRecordID is the separate stored replay-response record, or empty
	// when the response could not be stored under current budgets/policy.
	ReplayRecordID string
	// SideEffects warns that replay is a new request that may repeat effects.
	SideEffects string
	StartedAt   time.Time
	FinishedAt  time.Time
	ErrorCode   string
}

// AuditEntry is the safe replay audit record. It contains only actor/action,
// IDs/generations, time and outcome: never URL values, headers or payloads.
type AuditEntry struct {
	Actor              string    `json:"actor"`
	Action             string    `json:"action"`
	ResourceID         string    `json:"resource_id"`
	CaptureID          string    `json:"capture_id"`
	OperationID        string    `json:"operation_id"`
	ResourceGeneration uint64    `json:"resource_generation"`
	RouteGeneration    uint64    `json:"route_generation"`
	TargetGeneration   uint64    `json:"target_generation"`
	OccurredAt         time.Time `json:"occurred_at"`
	Outcome            string    `json:"outcome"`
	StatusCode         int       `json:"status_code"`
}

type storedReplayOp struct {
	fingerprint string
	result      ReplayResult
	outcome     string
	statusCode  int
	replayErr   error
	expiresAt   time.Time
	done        chan struct{}
	finished    bool
}

// Manager owns deliberate replay concurrency, idempotency and audit on top of
// one shared Store. It performs no network I/O itself; all origin contact
// goes through the caller-supplied target-bound ReplayForwarder.
type Manager struct {
	store *Store

	mu          sync.Mutex
	active      int
	perResource map[string]int
	ops         map[string]*storedReplayOp
	audit       []AuditEntry
	wake        chan struct{}
	stopped     bool
}

func NewManager(store *Store) *Manager {
	if store == nil {
		return nil
	}
	return &Manager{
		store:       store,
		wake:        make(chan struct{}, 1),
		perResource: make(map[string]int),
		ops:         make(map[string]*storedReplayOp),
	}
}

// Run expires safe replay outcomes and audit entries even while the daemon is idle.
func (m *Manager) Run(ctx context.Context) {
	if m == nil || ctx == nil {
		return
	}
	for {
		m.mu.Lock()
		now := m.store.currentTime()
		m.expireLocked(now)
		var next time.Time
		for _, op := range m.ops {
			if op.finished && (next.IsZero() || op.expiresAt.Before(next)) {
				next = op.expiresAt
			}
		}
		for _, entry := range m.audit {
			expiry := entry.OccurredAt.Add(Retention)
			if next.IsZero() || expiry.Before(next) {
				next = expiry
			}
		}
		m.mu.Unlock()
		var timer *time.Timer
		var tick <-chan time.Time
		if !next.IsZero() {
			timer = time.NewTimer(next.Sub(now))
			tick = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			m.mu.Lock()
			m.stopped = true
			// In-flight callers own their reservation until their cancellation completes.
			for key, op := range m.ops {
				if op.finished {
					delete(m.ops, key)
				}
			}
			clear(m.audit)
			m.audit = nil
			m.mu.Unlock()
			return
		case <-m.wake:
		case <-tick:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func validIdempotencyKey(value string) bool {
	return validResourceID(value)
}

func replayFingerprint(req ReplayRequest) string {
	return strings.Join([]string{
		req.ResourceID, req.CaptureID, req.PrincipalID,
		uint64String(req.ResourceGeneration), uint64String(req.RouteGeneration), uint64String(req.TargetGeneration),
	}, "\x00")
}

func uint64String(value uint64) string {
	if value == 0 {
		return "0"
	}
	const digits = "0123456789"
	var buf [20]byte
	pos := len(buf)
	for value > 0 {
		pos--
		buf[pos] = digits[value%10]
		value /= 10
	}
	return string(buf[pos:])
}

func newReplayOperationID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic("inspector: crypto/rand unavailable")
	}
	return ReplayOperationIDPrefix + hex.EncodeToString(value[:])
}

func (m *Manager) expireLocked(now time.Time) {
	for key, op := range m.ops {
		if op.finished && !now.Before(op.expiresAt) {
			delete(m.ops, key)
		}
	}
	kept := m.audit[:0]
	for _, entry := range m.audit {
		if now.Before(entry.OccurredAt.Add(Retention)) {
			kept = append(kept, entry)
		}
	}
	clear(m.audit[len(kept):])
	m.audit = kept
}

// Audit returns a copy of the bounded safe audit log, oldest first.
func (m *Manager) Audit() []AuditEntry {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(m.store.currentTime())
	out := make([]AuditEntry, len(m.audit))
	copy(out, m.audit)
	return out
}

func operationKey(req ReplayRequest) string { return req.PrincipalID + "\x00" + req.IdempotencyKey }

// Lookup returns an already admitted operation, including ambiguous results,
// without requiring the original raw capture or a live origin binding. The
// transport must still verify the caller's current exact replay grant first.
func (m *Manager) Lookup(ctx context.Context, req ReplayRequest) (ReplayResult, error, bool) {
	if m == nil || ctx == nil {
		return ReplayResult{}, ErrInvalid, true
	}
	m.mu.Lock()
	m.expireLocked(m.store.currentTime())
	op, ok := m.ops[operationKey(req)]
	if !ok {
		m.mu.Unlock()
		return ReplayResult{}, nil, false
	}
	if op.fingerprint != replayFingerprint(req) {
		m.mu.Unlock()
		return ReplayResult{}, ErrReplayConflict, true
	}
	done := op.done
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		return ReplayResult{}, ctx.Err(), true
	case <-done:
		return op.result, op.replayErr, true
	}
}

// Replay executes one deliberate audited replay. Validation failures return
// before any origin contact and consume no idempotency key. Once the
// forwarder is invoked, the outcome (including transport ambiguity) is stored
// under the idempotency key and duplicates return it without a second origin
// request. Losing an ambiguous result never retries implicitly.
func (m *Manager) Replay(ctx context.Context, req ReplayRequest, forward ReplayForwarder) (ReplayResult, error) {
	if m == nil || m.store == nil || forward == nil {
		return ReplayResult{}, ErrInvalid
	}
	if ctx == nil {
		return ReplayResult{}, ErrInvalid
	}
	if !validResourceID(req.ResourceID) || strings.TrimSpace(req.CaptureID) == "" || len(req.CaptureID) > 256 || !validIdempotencyKey(req.IdempotencyKey) || strings.TrimSpace(req.PrincipalID) == "" {
		return ReplayResult{}, ErrInvalid
	}
	now := m.store.currentTime()
	if req.AuthorityReadAt.IsZero() || req.ExpiresAt.IsZero() || !req.ExpiresAt.After(now) || now.Sub(req.AuthorityReadAt) > AuthorityFreshness {
		return ReplayResult{}, ErrStaleAuthority
	}

	deadline := now.Add(ReplayTimeout)
	if req.ExpiresAt.Before(deadline) {
		deadline = req.ExpiresAt
	}
	forwardCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err := forwardCtx.Err(); err != nil {
		return ReplayResult{}, err
	}

	// Reserve both the action key and audit/outcome capacity atomically before
	// dispatch. Concurrent duplicates wait for this operation; no second caller
	// can pass a stale lookup and dispatch after the first releases its slot.
	m.mu.Lock()
	m.expireLocked(now)
	if m.stopped {
		m.mu.Unlock()
		return ReplayResult{}, ErrDisabled
	}
	key := operationKey(req)
	if _, exists := m.ops[key]; exists {
		m.mu.Unlock()
		result, err, _ := m.Lookup(forwardCtx, req)
		return result, err
	}
	if len(m.ops) >= ReplayAuditMaxEntries {
		m.mu.Unlock()
		return ReplayResult{}, ErrNoAuditCapacity
	}
	if m.active >= ReplayMaxConcurrentDaemon || m.perResource[req.ResourceID] >= ReplayMaxConcurrentPerResource {
		m.mu.Unlock()
		return ReplayResult{}, ErrTooManyReads
	}
	op := &storedReplayOp{fingerprint: replayFingerprint(req), done: make(chan struct{}), expiresAt: now.Add(Retention)}
	m.ops[key] = op
	m.active++
	m.perResource[req.ResourceID]++
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.active--
		m.perResource[req.ResourceID]--
		if m.perResource[req.ResourceID] == 0 {
			delete(m.perResource, req.ResourceID)
		}
		if m.stopped {
			delete(m.ops, key)
		}
		if !op.finished {
			// Pre-dispatch failure leaves no retained action key or audit entry.
			op.replayErr = ErrReplayIneligible
			delete(m.ops, key)
		}
		close(op.done)
		m.mu.Unlock()
	}()

	credential := Credential{
		PrincipalID:        req.PrincipalID,
		Action:             ActionReplay,
		ResourceID:         req.ResourceID,
		ResourceGeneration: req.ResourceGeneration,
		RouteGeneration:    req.RouteGeneration,
		TargetGeneration:   req.TargetGeneration,
		AuthorityReadAt:    req.AuthorityReadAt,
		ExpiresAt:          req.ExpiresAt,
	}
	record, raw, err := m.fetchReplayable(forwardCtx, credential, req.CaptureID)
	if err != nil {
		return ReplayResult{}, err
	}
	method, requestURI, header, body, err := parseRawRequest(raw)
	if err != nil {
		return ReplayResult{}, err
	}
	if !supportedReplayMethod(method, header, record) {
		return ReplayResult{}, &IneligibleError{Reason: "unsupported"}
	}

	started := m.store.currentTime()
	status, responseHeader, responseBody, responseTruncated, forwardErr := forward(forwardCtx, method, requestURI, header, body)
	finished := m.store.currentTime()

	if forwardErr != nil {
		// The origin may have received the request before the failure. Report
		// ambiguity honestly and store it under the key so a retry with the
		// same key cannot open a second origin request.
		result := ReplayResult{
			OperationID: newReplayOperationID(),
			CaptureID:   record.ID,
			ResourceID:  record.ResourceID,
			Method:      record.Method,
			URL:         record.URL,
			StartedAt:   started,
			FinishedAt:  finished,
			ErrorCode:   "origin_unavailable",
			SideEffects: replaySideEffects(record.Method),
		}
		m.storeReplayOp(now, req, result, "ambiguous", 0, ErrAmbiguousReplay)
		return result, ErrAmbiguousReplay
	}

	completed := CompletedCapture{
		Method:             record.Method,
		RawURL:             record.URL,
		RequestHeaders:     record.RequestHeaders,
		RequestBody:        record.RequestBody,
		ResponseHeaders:    responseHeader,
		ResponseBody:       responseBody,
		ResponseTruncated:  responseTruncated,
		ResponseStatus:     status,
		StartedAt:          started,
		FinishedAt:         finished,
		ResourceGeneration: req.ResourceGeneration,
		RouteGeneration:    req.RouteGeneration,
		TargetGeneration:   req.TargetGeneration,
	}
	if forwardCtx.Err() != nil {
		completed.ErrorCode = "forwarding_failed"
	}
	replayRecordID := m.storeReplayResponse(req.ResourceID, completed)

	result := ReplayResult{
		OperationID:       newReplayOperationID(),
		CaptureID:         record.ID,
		ResourceID:        record.ResourceID,
		Method:            record.Method,
		URL:               record.URL,
		RequestBodyState:  record.RequestBodyState,
		ResponseStatus:    status,
		ResponseBodyState: responseCaptureState(responseHeader, responseBody, responseTruncated),
		ReplayRecordID:    replayRecordID,
		SideEffects:       replaySideEffects(record.Method),
		StartedAt:         started,
		FinishedAt:        finished,
		ErrorCode:         errorCodeOrNone(completed.ErrorCode),
	}
	// Provider signature expiry and other origin rejections are origin
	// results: surface the status without bypassing, re-signing or claiming
	// provider redelivery.
	m.storeReplayOp(now, req, result, "succeeded", status, nil)
	return result, nil
}

// fetchReplayable enforces replay-grant authorization, generation fencing,
// freshness/expiry, revocation and raw retention with honest typed reasons.
// It mirrors GetRaw without collapsing every failure to ErrExpired.
func (m *Manager) fetchReplayable(ctx context.Context, credential Credential, id string) (*Record, []byte, error) {
	if id == "" {
		return nil, nil, ErrInvalid
	}
	if err := m.store.acquireRead(ctx); err != nil {
		return nil, nil, err
	}
	defer m.store.releaseRead()
	m.store.mu.Lock()
	defer m.store.mu.Unlock()
	now := m.store.currentTime()
	m.store.expireLocked(now)
	if _, err := m.store.authorizeRetrieval(credential, ActionReplay); err != nil {
		return nil, nil, err
	}
	record, ok := m.store.records[id]
	if !ok || record.ResourceID != credential.ResourceID {
		return nil, nil, ErrNotFound
	}
	if staleRecord(record, credential) {
		return nil, nil, ErrStaleGeneration
	}
	if record.ReplayIneligible != "" {
		return nil, nil, &IneligibleError{Reason: record.ReplayIneligible}
	}
	entry, ok := m.store.raw[id]
	if !ok {
		return nil, nil, &IneligibleError{Reason: "raw_missing"}
	}
	if now.After(entry.expiresAt) {
		delete(m.store.raw, id)
		if record.ReplayIneligible == "" {
			record.ReplayIneligible = "raw_expired"
		}
		return nil, nil, &IneligibleError{Reason: "raw_expired"}
	}
	raw := make([]byte, len(entry.bytes))
	copy(raw, entry.bytes)
	copied := *record
	return &copied, raw, nil
}

func (m *Manager) storeReplayOp(now time.Time, req ReplayRequest, result ReplayResult, outcome string, status int, replayErr error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(m.store.currentTime())
	op := m.ops[operationKey(req)]
	op.result, op.outcome, op.statusCode, op.replayErr = result, outcome, status, replayErr
	op.finished = true
	select {
	case m.wake <- struct{}{}:
	default:
	}

	if m.stopped {
		return
	}
	m.audit = append(m.audit, AuditEntry{
		Actor:              req.PrincipalID,
		Action:             "replay",
		ResourceID:         req.ResourceID,
		CaptureID:          result.CaptureID,
		OperationID:        result.OperationID,
		ResourceGeneration: req.ResourceGeneration,
		RouteGeneration:    req.RouteGeneration,
		TargetGeneration:   req.TargetGeneration,
		OccurredAt:         now,
		Outcome:            outcome,
		StatusCode:         status,
	})
}

// storeReplayResponse stores the replay response as a separate record governed
// by the same budgets. It never attaches raw bytes, so a replay response is
// honestly reported as non-replayable instead of chaining replays. A dropped
// or disabled store reports an empty ID without failing the replay itself:
// forwarding already completed.
func (m *Manager) storeReplayResponse(resourceID string, completed CompletedCapture) string {
	pending, err := m.store.TryBegin(resourceID)
	if err != nil {
		return ""
	}
	record, err := m.store.Finish(pending, completed)
	if err != nil || record == nil {
		return ""
	}
	return record.ID
}

func replaySideEffects(method string) string {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return "Replay sends one new read-only request to the same origin; it may still record access in origin logs."
	default:
		return "Replay sends one new request to the same origin and may repeat side effects; it never re-signs provider signatures or redirects elsewhere."
	}
}

func responseCaptureState(header http.Header, body []byte, truncated bool) CaptureState {
	if truncated {
		return StateTruncated
	}
	if len(body) == 0 {
		return StateComplete
	}
	contentType := ""
	if header != nil {
		contentType = header.Get("Content-Type")
	}
	if !jsonBodySupported(contentType, body) {
		return StateUnsupported
	}
	return StateComplete
}

func supportedReplayMethod(method string, header http.Header, record *Record) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
	default:
		return false
	}
	if record != nil && record.ResponseStatus == http.StatusSwitchingProtocols {
		return false
	}
	if header != nil {
		if header.Get("Upgrade") != "" {
			return false
		}
		for _, value := range header.Values("Connection") {
			for _, token := range strings.Split(value, ",") {
				if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
					return false
				}
			}
		}
	}
	return true
}

// parseRawRequest splits the retained exact request into origin-form target,
// application headers and body. Hop-by-hop framing and reserved Paperboat
// credentials are removed; transport framing is recomputed by the forwarder.
// Host is retained as application authority and moved to Request.Host by the forwarder.
func parseRawRequest(raw []byte) (method, requestURI string, header http.Header, body []byte, err error) {
	separator := bytes.Index(raw, []byte("\r\n\r\n"))
	if separator < 0 {
		return "", "", nil, nil, ErrInvalid
	}
	head, body := raw[:separator], append([]byte(nil), raw[separator+4:]...)
	lines := bytes.Split(head, []byte("\r\n"))
	if len(lines) < 1 {
		return "", "", nil, nil, ErrInvalid
	}
	parts := bytes.SplitN(lines[0], []byte(" "), 3)
	if len(parts) != 3 || len(parts[0]) == 0 || len(parts[1]) == 0 {
		return "", "", nil, nil, ErrInvalid
	}
	method = strings.ToUpper(strings.TrimSpace(string(parts[0])))
	requestURI = strings.TrimSpace(string(parts[1]))
	if !strings.HasPrefix(requestURI, "/") {
		return "", "", nil, nil, ErrInvalid
	}
	if len(requestURI) > URLMaxBytes {
		return "", "", nil, nil, ErrInvalid
	}
	header = make(http.Header)
	connectionTokens := map[string]struct{}{}
	for _, line := range lines[1:] {
		if len(line) == 0 {
			continue
		}
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			return "", "", nil, nil, ErrInvalid
		}
		name := strings.TrimSpace(string(line[:colon]))
		value := strings.TrimSpace(string(line[colon+1:]))
		if name == "" || len(name) > 256 || len(value) > 8192 {
			return "", "", nil, nil, ErrInvalid
		}
		if len(header)+len(connectionTokens) > MaxHeadersPerDirection+16 {
			return "", "", nil, nil, ErrInvalid
		}
		header.Add(http.CanonicalHeaderKey(name), value)
	}
	for _, value := range header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			token = strings.TrimSpace(token)
			if token != "" {
				connectionTokens[http.CanonicalHeaderKey(token)] = struct{}{}
			}
		}
	}
	for name := range header {
		canonical := http.CanonicalHeaderKey(name)
		lower := strings.ToLower(canonical)
		if _, listed := connectionTokens[canonical]; listed {
			header.Del(canonical)
			continue
		}
		switch lower {
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade", "content-length":
			header.Del(canonical)
			continue
		}
		if strings.HasPrefix(lower, "x-paperboat-") || strings.HasPrefix(lower, "paperboat-") {
			header.Del(canonical)
			continue
		}
	}
	if int64(len(body)) > RawMaxBytes {
		return "", "", nil, nil, ErrInvalid
	}
	return method, requestURI, header, body, nil
}
