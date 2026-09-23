// Package inspectorapi owns the daemon-local inspector HTTP surface: bounded
// sanitized retrieval and deliberate audited replay for HTTP(S) tunnel
// captures. It serves literal loopback only; every call carries a
// server-minted inspector grant token identifying its principal, and every
// operation re-verifies that grant against live server authority before
// touching the store. Nothing is archived at the control plane or edge;
// every response is no-store. The daemon owner token is never accepted here
// and teammates never receive it.
package inspectorapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/inspector"
)

const (
	SchemaV1            = "paperboat.inspector/v1"
	maxRequestBytes     = 64 << 10
	maxPolicyNames      = 32
	maxSensitiveNameLen = 128
	// grantHeader carries the server-minted inspector grant token. It is
	// write-only: accepted on input, never logged, never returned.
	grantHeader = "X-Paperboat-Inspector-Grant"
)

var (
	ErrInvalid = errors.New("invalid inspector request")
	// ErrDenied reports a grant failure (unknown, revoked, expired, stale or
	// unauthorized). Callers may retry once with a freshly issued credential;
	// repeated denial must surface, never loop.
	ErrDenied = errors.New("inspector grant denied")
	// ErrUpstream reports an unreachable authority backend. Callers must not
	// treat it as a denial and must not retry issuance for it.
	ErrUpstream = errors.New("inspector authority unavailable")
)

// Decision is one live server authorization for a presented grant token.
// Freshness comes from the server (IssuedAt/ExpiresAt, 10-second decisions),
// never from daemon-local clocks.
type Decision struct {
	Principal          string
	Owner              string
	ResourceKind       string
	ResourceID         string
	RouteID            string
	ResourceGeneration uint64
	RouteGeneration    uint64
	TargetGeneration   uint64
	CredentialID       string
	IssuedAt           time.Time
	ExpiresAt          time.Time
}

// AuthorizeFunc re-verifies one grant token against live server authority for
// an exact resource/route/action. Implementations must fail closed on every
// drift and must never mint authority locally.
type AuthorizeFunc func(ctx context.Context, grantToken, kind, resource, route, action string) (Decision, error)

// Service composes one shared Store, its replay Manager and the current
// target-bound forwarder Registry behind per-principal server decisions.
type Service struct {
	store       *inspector.Store
	manager     *inspector.Manager
	registry    *inspector.Registry
	authorize   AuthorizeFunc
	now         func() time.Time
	reads       chan struct{}
	lifecycleMu sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}
}

type Config struct {
	Store     *inspector.Store
	Manager   *inspector.Manager
	Registry  *inspector.Registry
	Authorize AuthorizeFunc
	Now       func() time.Time
}

func New(config Config) (*Service, error) {
	if config.Store == nil || config.Registry == nil || config.Authorize == nil {
		return nil, ErrInvalid
	}
	manager := config.Manager
	if manager == nil {
		manager = inspector.NewManager(config.Store)
	}
	if manager == nil {
		return nil, ErrInvalid
	}
	now := config.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{store: config.Store, manager: manager, registry: config.Registry, authorize: config.Authorize, now: now, reads: make(chan struct{}, inspector.MaxConcurrentReads)}, nil
}

// Start and Shutdown tie all retention work to the stable daemon, including
// idle periods with no traffic or inspector clients.
func (s *Service) Start(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil {
		return ErrInvalid
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.cancel != nil {
		return ErrInvalid
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel, s.done = cancel, make(chan struct{})
	go func() {
		defer close(s.done)
		managerDone := make(chan struct{})
		go func() { defer close(managerDone); s.manager.Run(runCtx) }()
		s.store.Run(runCtx)
		<-managerDone
	}()
	return nil
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.lifecycleMu.Lock()
	cancel, done := s.cancel, s.done
	s.lifecycleMu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Store exposes the shared capture store for forwarding owners. It never
// leaves the daemon process.
func (s *Service) Store() *inspector.Store { return s.store }

// Registry exposes the current replay bindings for forwarding owners.
func (s *Service) Registry() *inspector.Registry { return s.registry }

// Manager exposes deliberate replay for tests.
func (s *Service) Manager() *inspector.Manager { return s.manager }

func (s *Service) currentTime() time.Time {
	if s == nil || s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

func loopbackOnly(request *http.Request) bool {
	if request == nil {
		return false
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(request.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(request.RemoteAddr)
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func (s *Service) authorized(request *http.Request) (string, bool) {
	if request == nil {
		return "", false
	}
	token := strings.TrimSpace(request.Header.Get(grantHeader))
	if token == "" || len(token) > 512 || strings.ContainsAny(token, "\x00\r\n\t ") {
		return "", false
	}
	return token, true
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	body, err := json.Marshal(value)
	if err != nil || len(body) > inspector.RetrievalMaxBytes {
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(writer, `{"schema":"paperboat.inspector/v1","kind":"error","code":"inspector_unavailable"}`)
		return
	}
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}

func writeError(writer http.ResponseWriter, status int, code string, extra map[string]string) {
	envelope := map[string]any{"schema": SchemaV1, "kind": "error", "code": code}
	for key, value := range extra {
		envelope[key] = value
	}
	writeJSON(writer, status, envelope)
}

func inspectorStatus(err error) (int, string, map[string]string) {
	var ineligible *inspector.IneligibleError
	switch {
	case err == nil:
		return http.StatusOK, "", nil
	case errors.Is(err, inspector.ErrInvalid) || errors.Is(err, ErrInvalid):
		return http.StatusBadRequest, "invalid_inspector_request", nil
	case errors.Is(err, inspector.ErrDisabled):
		return http.StatusNotFound, "inspector_not_enabled", nil
	case errors.Is(err, inspector.ErrForbidden) || errors.Is(err, ErrDenied):
		return http.StatusForbidden, "inspector_forbidden", nil
	case errors.Is(err, inspector.ErrStaleAuthority):
		return http.StatusUnauthorized, "inspector_authority_stale", nil
	case errors.Is(err, inspector.ErrStaleGeneration):
		return http.StatusConflict, "inspector_generation_stale", nil
	case errors.Is(err, inspector.ErrNotFound):
		return http.StatusNotFound, "inspector_not_found", nil
	case errors.Is(err, inspector.ErrExpired):
		return http.StatusGone, "inspector_expired", nil
	case errors.As(err, &ineligible):
		return http.StatusUnprocessableEntity, "inspector_replay_ineligible", map[string]string{"reason": ineligible.Reason}
	case errors.Is(err, inspector.ErrReplayConflict):
		return http.StatusConflict, "inspector_replay_conflict", nil
	case errors.Is(err, inspector.ErrTooManyReads):
		return http.StatusTooManyRequests, "inspector_busy", nil
	case errors.Is(err, inspector.ErrNoAuditCapacity):
		return http.StatusServiceUnavailable, "inspector_audit_unavailable", nil
	case errors.Is(err, inspector.ErrAmbiguousReplay):
		return http.StatusConflict, "inspector_replay_ambiguous", nil
	case errors.Is(err, inspector.ErrDropped):
		return http.StatusServiceUnavailable, "inspector_over_budget", nil
	case errors.Is(err, ErrUpstream):
		return http.StatusServiceUnavailable, "inspector_unavailable", nil
	default:
		return http.StatusInternalServerError, "inspector_unavailable", nil
	}
}

func decodeStrict(body io.Reader, out any) error {
	if body == nil {
		return ErrInvalid
	}
	raw, err := io.ReadAll(io.LimitReader(body, maxRequestBytes+1))
	if err != nil || int64(len(raw)) > maxRequestBytes {
		return ErrInvalid
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return ErrInvalid
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ErrInvalid
	}
	return nil
}

type policyRequest struct {
	ResourceKind        string   `json:"resource_kind"`
	ResourceID          string   `json:"resource_id"`
	RouteID             string   `json:"route_id"`
	Enabled             bool     `json:"enabled"`
	CaptureRequestBody  bool     `json:"capture_request_body"`
	CaptureResponseBody bool     `json:"capture_response_body"`
	CaptureRaw          bool     `json:"capture_raw"`
	SensitiveNames      []string `json:"sensitive_names"`
}

type replayHTTPRequest struct {
	ResourceKind   string `json:"resource_kind"`
	ResourceID     string `json:"resource_id"`
	RouteID        string `json:"route_id"`
	CaptureID      string `json:"capture_id"`
	IdempotencyKey string `json:"idempotency_key"`
}

type recordWire struct {
	Schema            string      `json:"schema"`
	Kind              string      `json:"kind"`
	ID                string      `json:"id"`
	ResourceID        string      `json:"resource_id"`
	ResourceGen       uint64      `json:"resource_generation"`
	RouteGen          uint64      `json:"route_generation"`
	TargetGen         uint64      `json:"target_generation"`
	Method            string      `json:"method"`
	URL               string      `json:"url"`
	URLTruncated      bool        `json:"url_truncated"`
	RequestHeaders    http.Header `json:"request_headers"`
	ResponseHeaders   http.Header `json:"response_headers"`
	HeadersTruncated  bool        `json:"headers_truncated"`
	RequestBody       string      `json:"request_body,omitempty"`
	RequestBodyState  string      `json:"request_body_state"`
	ResponseBody      string      `json:"response_body,omitempty"`
	ResponseBodyState string      `json:"response_body_state"`
	ResponseStatus    int         `json:"response_status"`
	ErrorCode         string      `json:"error_code"`
	State             string      `json:"state"`
	ReplayIneligible  string      `json:"replay_ineligible,omitempty"`
	StartedAt         time.Time   `json:"started_at"`
	FinishedAt        time.Time   `json:"finished_at"`
}

func wireRecord(record inspector.Record) recordWire {
	return recordWire{
		Schema: SchemaV1, Kind: "capture",
		ID: record.ID, ResourceID: record.ResourceID,
		ResourceGen: record.ResourceGeneration, RouteGen: record.RouteGeneration, TargetGen: record.TargetGeneration,
		Method: record.Method, URL: record.URL, URLTruncated: record.URLTruncated,
		RequestHeaders: record.RequestHeaders, ResponseHeaders: record.ResponseHeaders,
		HeadersTruncated: record.HeadersTruncated,
		RequestBody:      string(record.RequestBody), RequestBodyState: string(record.RequestBodyState),
		ResponseBody: string(record.ResponseBody), ResponseBodyState: string(record.ResponseBodyState),
		ResponseStatus: record.ResponseStatus, ErrorCode: record.ErrorCode,
		State: string(record.State), ReplayIneligible: record.ReplayIneligible,
		StartedAt: record.StartedAt, FinishedAt: record.FinishedAt,
	}
}

// ServeHTTP routes the daemon-local inspector API. Every route requires
// literal loopback and a server-minted grant token; the daemon owner token is
// never accepted here.
func (s *Service) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if !loopbackOnly(request) {
		writeError(writer, http.StatusUnauthorized, "inspector_unauthorized", nil)
		return
	}
	s.ServeAuthenticatedHTTP(writer, request)
}

// ServeAuthenticatedHTTP is mounted only by the dedicated authenticated native
// and connector inspector stream adapters. It never accepts an origin target;
// every operation still resolves the exact inspector grant at the control plane.
func (s *Service) ServeAuthenticatedHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	if s == nil || s.store == nil || s.authorize == nil || request == nil {
		writeError(writer, http.StatusUnauthorized, "inspector_unauthorized", nil)
		return
	}
	if _, ok := s.authorized(request); !ok {
		writeError(writer, http.StatusUnauthorized, "inspector_unauthorized", nil)
		return
	}
	if request.Method == http.MethodGet {
		select {
		case s.reads <- struct{}{}:
			defer func() { <-s.reads }()
		default:
			writeError(writer, http.StatusTooManyRequests, "inspector_busy", nil)
			return
		}
		// Keep the read slot until the bounded response has been written, not
		// merely until the in-memory store has produced its copy.
		controller := http.NewResponseController(writer)
		_ = controller.SetWriteDeadline(time.Now().Add(inspector.SlowReadTimeout))
		defer controller.SetWriteDeadline(time.Time{})
	}
	path := strings.Trim(strings.TrimPrefix(request.URL.Path, "/v1/inspector"), "/")
	switch {
	case request.Method == http.MethodPut && path == "policy":
		s.handlePolicy(writer, request)
	case request.Method == http.MethodGet && path == "records":
		s.handleList(writer, request)
	case request.Method == http.MethodGet && strings.HasPrefix(path, "records/"):
		s.handleGet(writer, request, strings.TrimPrefix(path, "records/"))
	case request.Method == http.MethodPost && path == "replay":
		s.handleReplay(writer, request)
	case request.Method == http.MethodDelete && path == "records":
		s.handlePurge(writer, request)
	case request.Method == http.MethodGet && path == "audit":
		s.handleAudit(writer, request)
	default:
		writeError(writer, http.StatusNotFound, "inspector_not_found", nil)
	}
}

func validQueryResource(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func (s *Service) handlePolicy(writer http.ResponseWriter, request *http.Request) {
	var body policyRequest
	if err := decodeStrict(request.Body, &body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_inspector_request", nil)
		return
	}
	if !validQueryResource(body.ResourceID) || len(body.SensitiveNames) > maxPolicyNames {
		writeError(writer, http.StatusBadRequest, "invalid_inspector_request", nil)
		return
	}
	for _, name := range body.SensitiveNames {
		if len(name) > maxSensitiveNameLen {
			writeError(writer, http.StatusBadRequest, "invalid_inspector_request", nil)
			return
		}
	}
	// Capture-policy changes require the replay action: they control future
	// replayability and destroy nothing by themselves, but only replay
	// grantees and the owner may reshape collection.
	credential, err := s.authorizeOp(request.Context(), request, body.ResourceKind, body.ResourceID, body.RouteID, inspector.ActionReplay)
	if err != nil {
		status, code, extra := inspectorStatus(err)
		writeError(writer, status, code, extra)
		return
	}
	policy := inspector.ResourcePolicy{
		Enabled: body.Enabled, CaptureRequestBody: body.CaptureRequestBody,
		CaptureResponseBody: body.CaptureResponseBody, CaptureRaw: body.CaptureRaw,
		SensitiveNames:     body.SensitiveNames,
		ResourceGeneration: credential.ResourceGeneration,
		RouteGeneration:    credential.RouteGeneration,
		TargetGeneration:   credential.TargetGeneration,
	}
	if err := s.store.SetPolicy(body.RouteID, policy); err != nil {
		status, code, extra := inspectorStatus(err)
		writeError(writer, status, code, extra)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"schema": SchemaV1, "kind": "policy", "resource_id": body.ResourceID, "enabled": body.Enabled})
}

// authorizeOp verifies the caller's grant token against live server authority
// for one exact resource/route/action and returns the store credential for
// that action. Generations and freshness come from the server decision; the
// daemon never mints authority locally.
func (s *Service) authorizeOp(ctx context.Context, request *http.Request, kind, resource, route string, action inspector.Action) (inspector.Credential, error) {
	if s == nil || s.authorize == nil {
		return inspector.Credential{}, ErrUpstream
	}
	if kind != "preview" && kind != "tunnel" {
		return inspector.Credential{}, ErrInvalid
	}
	if !validQueryResource(resource) || !validQueryResource(route) {
		return inspector.Credential{}, ErrInvalid
	}
	if kind == "preview" && route != resource {
		return inspector.Credential{}, ErrInvalid
	}
	token, ok := s.authorized(request)
	if !ok {
		return inspector.Credential{}, ErrDenied
	}
	decision, err := s.authorize(ctx, token, kind, resource, route, string(action))
	if err != nil {
		return inspector.Credential{}, err
	}
	now := s.currentTime()
	if decision.Principal == "" || decision.ResourceKind != kind || decision.ResourceID != resource || decision.RouteID != route ||
		decision.ResourceGeneration == 0 || decision.RouteGeneration == 0 || decision.TargetGeneration == 0 ||
		decision.IssuedAt.IsZero() || decision.ExpiresAt.IsZero() || !decision.ExpiresAt.After(decision.IssuedAt) ||
		!decision.ExpiresAt.After(now) || now.Sub(decision.IssuedAt) > inspector.AuthorityFreshness {
		return inspector.Credential{}, ErrDenied
	}
	return inspector.Credential{
		PrincipalID:        decision.Principal,
		Action:             action,
		ResourceID:         route,
		ResourceGeneration: decision.ResourceGeneration,
		RouteGeneration:    decision.RouteGeneration,
		TargetGeneration:   decision.TargetGeneration,
		AuthorityReadAt:    decision.IssuedAt,
		ExpiresAt:          decision.ExpiresAt,
	}, nil
}

func (s *Service) handleList(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	kind := strings.TrimSpace(query.Get("kind"))
	resourceID := strings.TrimSpace(query.Get("resource"))
	routeID := strings.TrimSpace(query.Get("route"))
	cursor := query.Get("cursor")
	limit := 20
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > inspector.RetrievalMaxRecords {
			writeError(writer, http.StatusBadRequest, "invalid_inspector_request", nil)
			return
		}
		limit = value
	}
	if len(cursor) > 256 || strings.ContainsAny(cursor, "\x00\r\n") {
		writeError(writer, http.StatusBadRequest, "invalid_inspector_request", nil)
		return
	}
	credential, err := s.authorizeOp(request.Context(), request, kind, resourceID, routeID, inspector.ActionInspect)
	if err != nil {
		status, code, extra := inspectorStatus(err)
		writeError(writer, status, code, extra)
		return
	}
	page, err := s.store.List(request.Context(), credential, cursor, limit)
	if err != nil {
		status, code, extra := inspectorStatus(err)
		writeError(writer, status, code, extra)
		return
	}
	records := make([]recordWire, 0, len(page.Records))
	// Store accounting includes retained raw bytes, while JSON display expands
	// escaped body/header text. Enforce the wire budget on encoded records so
	// a valid store page cannot turn into a 500 during serialization.
	envelope := map[string]any{"schema": SchemaV1, "kind": "capture_page", "resource_id": resourceID, "records": []recordWire{}, "next_cursor": strings.Repeat("x", 256)}
	base, _ := json.Marshal(envelope)
	wireBytes := len(base)
	for _, record := range page.Records {
		wire := wireRecord(record)
		encoded, err := json.Marshal(wire)
		if err != nil {
			writeError(writer, http.StatusInternalServerError, "inspector_unavailable", nil)
			return
		}
		if wireBytes+len(encoded)+1 > inspector.RetrievalMaxBytes {
			if len(records) == 0 {
				writeError(writer, http.StatusInternalServerError, "inspector_unavailable", nil)
				return
			}
			page.NextCursor = records[len(records)-1].ID
			break
		}
		wireBytes += len(encoded) + 1
		records = append(records, wire)
	}
	writeJSON(writer, http.StatusOK, map[string]any{"schema": SchemaV1, "kind": "capture_page", "resource_id": resourceID, "records": records, "next_cursor": page.NextCursor})
}

func (s *Service) handleGet(writer http.ResponseWriter, request *http.Request, id string) {
	query := request.URL.Query()
	kind := strings.TrimSpace(query.Get("kind"))
	resourceID := strings.TrimSpace(query.Get("resource"))
	routeID := strings.TrimSpace(query.Get("route"))
	if strings.TrimSpace(id) == "" || len(id) > 256 || strings.ContainsAny(id, "\x00\r\n/") {
		writeError(writer, http.StatusBadRequest, "invalid_inspector_request", nil)
		return
	}
	credential, err := s.authorizeOp(request.Context(), request, kind, resourceID, routeID, inspector.ActionInspect)
	if err != nil {
		status, code, extra := inspectorStatus(err)
		writeError(writer, status, code, extra)
		return
	}
	record, err := s.store.Get(request.Context(), credential, id)
	if err != nil {
		status, code, extra := inspectorStatus(err)
		writeError(writer, status, code, extra)
		return
	}
	writeJSON(writer, http.StatusOK, wireRecord(record))
}

func (s *Service) handleReplay(writer http.ResponseWriter, request *http.Request) {
	var body replayHTTPRequest
	if err := decodeStrict(request.Body, &body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_inspector_request", nil)
		return
	}
	if strings.TrimSpace(body.CaptureID) == "" || len(body.CaptureID) > 256 {
		writeError(writer, http.StatusBadRequest, "invalid_inspector_request", nil)
		return
	}
	credential, err := s.authorizeOp(request.Context(), request, body.ResourceKind, body.ResourceID, body.RouteID, inspector.ActionReplay)
	if err != nil {
		status, code, extra := inspectorStatus(err)
		writeError(writer, status, code, extra)
		return
	}
	replayReq := inspector.ReplayRequest{
		PrincipalID:        credential.PrincipalID,
		ResourceID:         credential.ResourceID,
		CaptureID:          body.CaptureID,
		ResourceGeneration: credential.ResourceGeneration,
		RouteGeneration:    credential.RouteGeneration,
		TargetGeneration:   credential.TargetGeneration,
		IdempotencyKey:     strings.TrimSpace(body.IdempotencyKey),
		AuthorityReadAt:    credential.AuthorityReadAt,
		ExpiresAt:          credential.ExpiresAt,
	}
	if replayReq.IdempotencyKey == "" {
		writeError(writer, http.StatusBadRequest, "invalid_inspector_request", nil)
		return
	}
	result, err, found := s.manager.Lookup(request.Context(), replayReq)
	if !found {
		binding, ok := s.registry.Current(credential.ResourceID, s.currentTime())
		if !ok {
			writeError(writer, http.StatusGone, "inspector_expired", nil)
			return
		}
		if binding.ResourceGeneration != credential.ResourceGeneration || binding.RouteGeneration != credential.RouteGeneration || binding.TargetGeneration != credential.TargetGeneration {
			writeError(writer, http.StatusConflict, "inspector_generation_stale", nil)
			return
		}
		if binding.ExpiresAt.Before(replayReq.ExpiresAt) {
			replayReq.ExpiresAt = binding.ExpiresAt
		}
		result, err = s.manager.Replay(request.Context(), replayReq, binding.Forward)
	}
	envelope := map[string]any{
		"schema": SchemaV1, "kind": "replay",
		"operation_id": result.OperationID, "capture_id": result.CaptureID, "resource_id": result.ResourceID,
		"method": result.Method, "url": result.URL, "request_body_state": string(result.RequestBodyState),
		"response_status": result.ResponseStatus, "response_body_state": string(result.ResponseBodyState),
		"replay_record_id": result.ReplayRecordID, "side_effects": result.SideEffects,
		"started_at": result.StartedAt, "finished_at": result.FinishedAt, "error_code": result.ErrorCode,
	}
	if err != nil {
		status, code, extra := inspectorStatus(err)
		if extra == nil {
			extra = map[string]string{}
		}
		envelope["code"] = code
		for key, value := range extra {
			envelope[key] = value
		}
		if result.OperationID != "" {
			writeJSON(writer, status, envelope)
			return
		}
		writeError(writer, status, code, extra)
		return
	}
	writeJSON(writer, http.StatusOK, envelope)
}

func (s *Service) handlePurge(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	kind := strings.TrimSpace(query.Get("kind"))
	resourceID := strings.TrimSpace(query.Get("resource"))
	routeID := strings.TrimSpace(query.Get("route"))
	credential, err := s.authorizeOp(request.Context(), request, kind, resourceID, routeID, inspector.ActionReplay)
	if err != nil {
		status, code, extra := inspectorStatus(err)
		writeError(writer, status, code, extra)
		return
	}
	s.registry.Unregister(credential.ResourceID)
	s.store.Revoke(credential.ResourceID)
	writeJSON(writer, http.StatusOK, map[string]any{"schema": SchemaV1, "kind": "purged", "resource_id": resourceID})
}

func (s *Service) handleAudit(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	kind := strings.TrimSpace(query.Get("kind"))
	resourceID := strings.TrimSpace(query.Get("resource"))
	routeID := strings.TrimSpace(query.Get("route"))
	limit := 20
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			writeError(writer, http.StatusBadRequest, "invalid_inspector_request", nil)
			return
		}
		limit = value
	}
	credential, err := s.authorizeOp(request.Context(), request, kind, resourceID, routeID, inspector.ActionInspect)
	if err != nil {
		status, code, extra := inspectorStatus(err)
		writeError(writer, status, code, extra)
		return
	}
	entries := s.manager.Audit()
	kept := make([]inspector.AuditEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.ResourceID == credential.ResourceID {
			kept = append(kept, entry)
		}
	}
	if len(kept) > limit {
		kept = kept[len(kept)-limit:]
	}
	writeJSON(writer, http.StatusOK, map[string]any{"schema": SchemaV1, "kind": "audit_page", "entries": kept})
}
