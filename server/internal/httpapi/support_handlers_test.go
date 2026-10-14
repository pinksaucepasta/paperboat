package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
	"github.com/pinksaucepasta/paperboat-server/internal/telemetry"
)

type supportTestStore struct {
	mu      sync.Mutex
	records []supportreport.Record
	fail    bool
	looked  bool
}

func (s *supportTestStore) Save(_ context.Context, r supportreport.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r)
	return nil
}
func (s *supportTestStore) Lookup(_ context.Context, actor, owner, ref string, operator bool) ([]supportreport.Record, error) {
	s.looked = true
	if s.fail {
		return nil, errors.New("audit unavailable")
	}
	return []supportreport.Record{{Reference: ref, OwnerID: owner, Code: "internal_error", EventID: "01234567890123456789012345678901"}}, nil
}
func (s *supportTestStore) Cleanup(context.Context) error { return nil }
func TestSupportLookupAuthorityAndDisclosure(t *testing.T) {
	ref := supportreport.NewReference()
	for _, role := range []auth.Role{auth.RoleUser, auth.RoleSupport, auth.RoleAdmin} {
		store := &supportTestStore{}
		service, _ := supportreport.New(store, supportreport.Config{})
		req := httptest.NewRequest("GET", "/v1/admin/support/"+ref+"?owner_id=usr_target", nil)
		req.SetPathValue("reference", ref)
		req.Pattern = "GET /v1/admin/support/{reference}"
		req = req.WithContext(context.WithValue(req.Context(), authContextKey{}, principal{User: auth.User{ID: "usr_operator", Role: role}, Session: auth.Session{OperatorAuthenticatedAt: time.Now()}}))
		rr := httptest.NewRecorder()
		operatorGuard(&operatorAuditFake{}, supportLookup(service, true, "https://sentry.example/issues/")).ServeHTTP(rr, req)
		if role == auth.RoleUser {
			if rr.Code != 403 || store.looked {
				t.Fatal("nonoperator accessed evidence")
			}
			continue
		}
		if rr.Code != 200 || !store.looked || !strings.Contains(rr.Body.String(), "sentry.example") {
			t.Fatalf("operator lookup: %d %s", rr.Code, rr.Body.String())
		}
		store.fail = true
		rr = httptest.NewRecorder()
		operatorGuard(&operatorAuditFake{}, supportLookup(service, true, "")).ServeHTTP(rr, req)
		if rr.Code != 503 || strings.Contains(rr.Body.String(), "012345678901") {
			t.Fatal("audit failure disclosed evidence")
		}
	}
	store := &supportTestStore{}
	service, _ := supportreport.New(store, supportreport.Config{})
	req := httptest.NewRequest("GET", "/v1/support/"+ref, nil)
	req.SetPathValue("reference", ref)
	req = req.WithContext(context.WithValue(req.Context(), authContextKey{}, principal{User: auth.User{ID: "usr_owner"}}))
	rr := httptest.NewRecorder()
	supportLookup(service, false, "").ServeHTTP(rr, req)
	if rr.Code != 200 || strings.Contains(rr.Body.String(), "sentry") || strings.Contains(rr.Body.String(), "internal_error") || strings.Contains(rr.Body.String(), "usr_owner") {
		t.Fatal("self lookup leaked internal evidence")
	}
}
func TestSupportReferenceAndPanicCapture(t *testing.T) {
	store := &supportTestStore{}
	service, _ := supportreport.New(store, supportreport.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { service.Run(ctx); close(done) }()
	defer func() {
		cancel()
		<-done
		store.mu.Lock()
		defer store.mu.Unlock()
		if len(store.records) != 1 {
			t.Errorf("panic reported %d times", len(store.records))
		}
	}()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := supportMiddleware(service, logger, timeout(time.Second, logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { bindSupportOwner(r.Context(), "usr_a"); panic("SECRET") })))
	ref := supportreport.NewReference()
	r := httptest.NewRequest("GET", "/secret/path?token=SECRET", nil)
	r.Header.Set("Support-Reference", ref)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 500 || w.Header().Get("Support-Reference") != ref || !strings.Contains(w.Body.String(), ref) || strings.Contains(w.Body.String(), "SECRET") {
		t.Fatalf("invalid error response: %s", w.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		store.mu.Lock()
		n := len(store.records)
		if n > 0 {
			first := store.records[0]
			store.mu.Unlock()
			if first.Code != "panic" || first.OwnerID != "usr_a" {
				t.Fatalf("initiating failure lost: %+v", first)
			}
			return
		}
		store.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("failure was not captured")
}

func TestReadinessFailureRetainsUnderlyingCause(t *testing.T) {
	store := &supportTestStore{}
	service, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = service.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()

	router := NewRouter(Options{
		Config: config.Default(), SupportReports: service,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		ReadinessChecker: readinessFunc(func(context.Context) error { return context.DeadlineExceeded }),
	})
	reference := supportreport.NewReference()
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/readyz", nil)
	request.RemoteAddr = "127.0.0.1:43210"
	request.Header.Set("Support-Reference", reference)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness status=%d body=%s", response.Code, response.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		store.mu.Lock()
		records := append([]supportreport.Record(nil), store.records...)
		store.mu.Unlock()
		if len(records) > 0 {
			fault := records[0]
			if fault.Cause != "deadline_exceeded" || fault.HTTPStatus != http.StatusServiceUnavailable || fault.Reference != reference {
				t.Fatalf("readiness cause/reference/status lost: %+v", fault)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("readiness failure was not reported")
}

func TestTimeoutPreservesDeadlineCauseAndIgnoresClientCancellation(t *testing.T) {
	store := &supportTestStore{}
	service, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancelReporter := context.WithCancel(context.Background())
	serviceDone := make(chan struct{})
	go func() { _ = service.Run(runCtx); close(serviceDone) }()
	defer func() { cancelReporter(); <-serviceDone }()

	deadlineRelease := make(chan struct{})
	deadlineStarted := make(chan struct{})
	deadlineHandlerDone := make(chan struct{})
	cancelRelease := make(chan struct{})
	cancelStarted := make(chan struct{})
	cancelHandlerDone := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/deadline":
			close(deadlineStarted)
			<-deadlineRelease
			close(deadlineHandlerDone)
			w.WriteHeader(http.StatusNoContent)
		case "/cancel":
			close(cancelStarted)
			<-cancelRelease
			close(cancelHandlerDone)
			w.WriteHeader(http.StatusNoContent)
		}
	})
	cfg := config.Default()
	cfg.HTTP.RequestTimeout = 30 * time.Millisecond
	router := NewRouter(Options{Config: cfg, SupportReports: service, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), OverrideHandler: handler})

	deadlineResponse := httptest.NewRecorder()
	router.ServeHTTP(deadlineResponse, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/deadline", nil))
	if deadlineResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("deadline status=%d body=%s", deadlineResponse.Code, deadlineResponse.Body.String())
	}
	close(deadlineRelease)
	select {
	case <-deadlineStarted:
	case <-time.After(time.Second):
		t.Fatal("deadline handler did not start")
	}
	select {
	case <-deadlineHandlerDone:
	case <-time.After(time.Second):
		t.Fatal("deadline handler did not exit")
	}

	cancelCtx, cancelRequest := context.WithCancel(context.Background())
	cancelResponse := httptest.NewRecorder()
	cancelRequestDone := make(chan struct{})
	go func() {
		router.ServeHTTP(cancelResponse, httptest.NewRequestWithContext(cancelCtx, http.MethodGet, "http://127.0.0.1/cancel", nil))
		close(cancelRequestDone)
	}()
	select {
	case <-cancelStarted:
	case <-time.After(time.Second):
		t.Fatal("cancellation handler did not start")
	}
	cancelRequest()
	select {
	case <-cancelRequestDone:
	case <-time.After(time.Second):
		t.Fatal("canceled request did not return")
	}
	close(cancelRelease)
	select {
	case <-cancelHandlerDone:
	case <-time.After(time.Second):
		t.Fatal("canceled handler did not exit")
	}

	cancelReporter()
	select {
	case <-serviceDone:
	case <-time.After(time.Second):
		t.Fatal("support reporter did not drain")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.records) != 1 {
		t.Fatalf("recorded %d failures, want only the server deadline", len(store.records))
	}
	if store.records[0].Cause != "deadline_exceeded" || store.records[0].HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("deadline cause was not preserved: %+v", store.records[0])
	}
	if cancelResponse.Code == http.StatusServiceUnavailable {
		t.Fatalf("client cancellation returned synthetic failure: %s", cancelResponse.Body.String())
	}
}

func TestTelemetryDiagnosticsSerializationFailureRetainsCause(t *testing.T) {
	store := &supportTestStore{}
	service, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = service.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()

	diagnostics, err := telemetry.NewDiagnostics(telemetry.DiagnosticsConfig{Now: func() time.Time { return time.Unix(12, 0).UTC() }})
	if err != nil {
		t.Fatal(err)
	}
	reference := supportreport.NewReference()
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/diagnostics", nil)
	request.RemoteAddr = "127.0.0.1:43210"
	request.Header.Set("Support-Reference", reference)
	response := httptest.NewRecorder()
	handler := supportMiddleware(service, slog.New(slog.NewTextHandler(io.Discard, nil)), telemetryDiagnostics(diagnostics))
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("diagnostics status=%d body=%s", response.Code, response.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		store.mu.Lock()
		records := append([]supportreport.Record(nil), store.records...)
		store.mu.Unlock()
		if len(records) > 0 {
			fault := records[0]
			errorChain := strings.Join(fault.ErrorChain, " ")
			if fault.Code != "diagnostics_unavailable" || fault.Cause != "service_unavailable" || !strings.Contains(errorChain, "telemetry.Error") || fault.HTTPStatus != http.StatusServiceUnavailable || fault.Reference != reference {
				t.Fatalf("diagnostics cause/reference/status lost: %+v", fault)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("diagnostics failure was not reported")
}
