package operatorreporting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

func TestSafeSentryDTOsAndPagination(t *testing.T) {
	raw := sentryEvent{EventID: "0123456789abcdef0123456789abcdef", Title: strings.Repeat("x", 600), Tags: []sentryTag{
		{Key: "support_reference", Value: "support_01234567-89ab-4def-8123-456789abcdef"},
		{Key: "component", Value: "paperboat-server"}, {Key: "code", Value: "private_token_sentinel"}, {Key: "request_body", Value: "secret"},
	}}
	event := raw.safe()
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if event.SupportReference == "" || strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "private_token_sentinel") || strings.Contains(string(encoded), strings.Repeat("x", 100)) {
		t.Fatalf("unsafe event: %s", encoded)
	}
	link := `<https://sentry.io/api/0/issues/?cursor=0:0:1>; rel="previous"; results="false", <https://sentry.io/api/0/issues/?cursor=0:100:0>; rel="next"; results="true"`
	if got := nextCursor(link); got != "0:100:0" {
		t.Fatalf("cursor=%q", got)
	}
	cursor := localCursor{Reference: "support_01234567-89ab-4def-8123-456789abcdef", OwnerID: "owner_1"}
	if got, ok := decodeLocalCursor(encodeLocalCursor(cursor)); !ok || got != cursor {
		t.Fatalf("local cursor=%+v ok=%v", got, ok)
	}
	project := sentryProject{ID: "99", Slug: "paperboat"}
	if !(sentryIssue{Project: project}).inProject(project) || (sentryIssue{Project: sentryProject{ID: "100", Slug: "other"}}).inProject(project) {
		t.Fatal("issue project boundary failed")
	}
}

func TestClassifyDBPreservesUnavailableCause(t *testing.T) {
	cause := errors.New("database token must not be logged")
	classified := classifyDB(cause)
	if !errors.Is(classified, ErrUnavailable) || !errors.Is(classified, cause) {
		t.Fatalf("classified error lost cause: %v", classified)
	}
}

func TestExpectedReportingErrorRequiresPureCompleteCause(t *testing.T) {
	if !reportingErrorOnly(fmt.Errorf("provider: %w", ErrNotFound), ErrNotFound) {
		t.Fatal("pure wrapped provider outcome was not recognized")
	}
	if !reportingErrorOnly(errors.Join(ErrNotFound, ErrNotFound), ErrNotFound) {
		t.Fatal("repeated pure outcome was mistaken for an error cycle")
	}
	for name, err := range map[string]error{
		"joined I/O failure": errors.Join(ErrNotFound, syscall.EIO),
		"unknown sibling":    errors.Join(ErrForbidden, errors.New("unexpected failure")),
	} {
		t.Run(name, func(t *testing.T) {
			if reportingErrorOnly(err, ErrNotFound, ErrForbidden) {
				t.Fatal("mixed failure was treated as an expected provider outcome")
			}
		})
	}
	cycle := &reportingCycleError{}
	cycle.next = errors.Join(ErrForbidden, cycle)
	if reportingErrorOnly(cycle, ErrForbidden) {
		t.Fatal("cyclic failure was treated as an expected outcome")
	}
	var typedNil *reportingCycleError
	if reportingErrorOnly(typedNil, ErrForbidden) {
		t.Fatal("typed-nil failure was treated as an expected outcome")
	}

	cause := syscall.EIO
	classified := classifyDB(errors.Join(ErrForbidden, cause))
	if !errors.Is(classified, ErrUnavailable) || !errors.Is(classified, cause) || reportingErrorOnly(classified, ErrForbidden) {
		t.Fatalf("mixed authorization/database failure was not kept operational: %v", classified)
	}
	pureCancel := classifyDB(fmt.Errorf("provider request canceled: %w", context.Canceled))
	if !errors.Is(pureCancel, context.Canceled) || errors.Is(pureCancel, ErrUnavailable) {
		t.Fatalf("pure wrapped cancellation was not preserved: %v", pureCancel)
	}
}

type reportingCycleError struct{ next error }

func (*reportingCycleError) Error() string   { return "cycle" }
func (e *reportingCycleError) Unwrap() error { return e.next }

func TestPreAuthorizePreservesClosedPoolCause(t *testing.T) {
	store, err := db.Open(config.Database{Driver: "postgres", DSN: "postgres://user:pass@127.0.0.1:1/paperboat?sslmode=disable"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	service := &Service{store: store, baseURL: &url.URL{Scheme: "https", Host: "sentry.example.test"}}
	err = service.preAuthorize(t.Context(), auth.User{ID: "usr_test"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("pre-authorization error = %v, want unavailable", err)
	}
	if err == ErrUnavailable {
		t.Fatal("pre-authorization discarded the closed-pool cause")
	}
}

func TestHTTPClientIsBoundedAndDoesNotRedirect(t *testing.T) {
	var token string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"1"}`))
		case "/rate":
			w.WriteHeader(http.StatusTooManyRequests)
		case "/redirect":
			http.Redirect(w, r, "https://example.invalid/", http.StatusFound)
		case "/large":
			_, _ = w.Write(make([]byte, maxResponseBytes+1))
		case "/server-error":
			w.WriteHeader(http.StatusBadGateway)
		case "/malformed-monitor":
			_, _ = w.Write([]byte(`[{"slug":"health","status":"active","type":"cron_job","project":{"id":"42"},"environments":[{"name":3}],"config":{"max_runtime":1,"failure_issue_threshold":1,"recovery_threshold":1}}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	s := &Service{baseURL: u, token: "top-secret", client: server.Client()}
	s.client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	var value map[string]any
	if _, err := s.get(t.Context(), "/ok", nil, &value); err != nil || value["id"] != "1" || token != "Bearer top-secret" {
		t.Fatalf("value=%v token=%q err=%v", value, token, err)
	}
	if _, err := s.get(t.Context(), "/rate", nil, &value); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("rate err=%v", err)
	}
	if _, err := s.get(t.Context(), "/redirect", nil, &value); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("redirect err=%v", err)
	}
	if _, err := s.get(t.Context(), "/large", nil, &value); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("large err=%v", err)
	}
	if _, err := s.get(t.Context(), "/server-error", nil, &value); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("provider failure err=%v", err)
	} else {
		var status interface{ DiagnosticStatus() int }
		if !errors.As(err, &status) || status.DiagnosticStatus() != http.StatusBadGateway {
			t.Fatalf("provider status cause was not retained: %v", err)
		}
	}
	var monitors []sentryMonitor
	if _, err := s.get(t.Context(), "/malformed-monitor", nil, &monitors); err == nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("malformed provider payload err=%v", err)
	} else {
		if strings.Contains(err.Error(), "cannot unmarshal") || strings.Contains(err.Error(), `"name":3`) {
			t.Fatal("malformed provider payload exposed parser or source data")
		}
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			t.Fatalf("provider decoder cause was lost: %T", err)
		}
	}
}

type errorRoundTripper struct{ err error }

func (r errorRoundTripper) RoundTrip(*http.Request) (*http.Response, error) { return nil, r.err }

func TestSentryTransportFailureRetainsCause(t *testing.T) {
	cause := errors.New("transport details must not be discarded")
	s := &Service{
		baseURL: &url.URL{Scheme: "https", Host: "sentry.example.test"},
		token:   "top-secret",
		client:  &http.Client{Transport: errorRoundTripper{err: cause}},
	}
	_, err := s.get(t.Context(), "/api/0/issues/", nil, new(any))
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, cause) {
		t.Fatalf("transport cause was lost: %v", err)
	}
}

func TestProviderRequestCancellationRetainsIndependentTransportCause(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cause := syscall.EIO
	s := &Service{
		baseURL: &url.URL{Scheme: "https", Host: "sentry.example.test"},
		client:  &http.Client{Transport: cancelRoundTripper{cancel: cancel, err: cause}},
	}
	_, err := s.get(ctx, "/api/0/issues/", nil, new(any))
	if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("cancellation masked transport cause: %v", err)
	}
}

type cancelRoundTripper struct {
	cancel context.CancelFunc
	err    error
}

func (r cancelRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	r.cancel()
	return nil, r.err
}

func TestProviderBodyCloseFailureRetainsCause(t *testing.T) {
	cause := syscall.EIO
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       closeErrorBody{Reader: strings.NewReader(`{"id":"safe"}`), closeErr: cause},
	}
	s := &Service{
		baseURL: &url.URL{Scheme: "https", Host: "sentry.example.test"},
		client:  &http.Client{Transport: responseRoundTripper{response: response}},
	}
	_, err := s.get(t.Context(), "/api/0/issues/", nil, new(map[string]string))
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, cause) {
		t.Fatalf("body close cause was lost: %v", err)
	}
}

type closeErrorBody struct {
	*strings.Reader
	closeErr error
}

func (b closeErrorBody) Close() error { return b.closeErr }

type responseRoundTripper struct{ response *http.Response }

func (r responseRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return r.response, nil
}

func TestMultiProjectConfigAndSafeLogProjection(t *testing.T) {
	if _, err := New(nil, nil, Config{}); !errors.Is(err, ErrInvalid) { // store is always required
		t.Fatalf("nil store err=%v", err)
	}
	allowed := map[string]string{"native": "101"}
	row := map[string]json.RawMessage{
		"id": json.RawMessage(`"abc"`), "project": json.RawMessage(`"native"`),
		"timestamp": json.RawMessage(`"2026-09-19T00:00:00Z"`), "severity": json.RawMessage(`"error"`),
		"support_reference": json.RawMessage(`"support_01234567-89ab-4def-8123-456789abcdef"`),
		"operation":         json.RawMessage(`"private_token_sentinel"`),
		"outcome":           json.RawMessage(`"private_token_sentinel"`),
		"dimension":         json.RawMessage(`"private_token_sentinel"`),
		"stage":             json.RawMessage(`"private_token_sentinel"`),
		"code":              json.RawMessage(`"private_token_sentinel"`),
		"message":           json.RawMessage(`"Bearer secret-must-not-leak"`),
	}
	got, ok := safeLog(row, allowed)
	b, _ := json.Marshal(got)
	if !ok || got.Project != "native" || strings.Contains(string(b), "secret-must-not-leak") || strings.Contains(string(b), "private_token_sentinel") {
		t.Fatalf("unsafe log %s ok=%v", b, ok)
	}
	row["operation"] = json.RawMessage(`"exec"`)
	row["outcome"] = json.RawMessage(`"success"`)
	row["code"] = json.RawMessage(`"operation_succeeded"`)
	row["dimension"] = json.RawMessage(`"service"`)
	got, ok = safeLog(row, allowed)
	if !ok || got.Operation != "exec" || got.Outcome != "success" || got.Code != "operation_succeeded" || got.Dimension != "service" {
		t.Fatalf("native command semantics lost: %+v ok=%v", got, ok)
	}
	row["operation"] = json.RawMessage(`"component_start"`)
	row["code"] = json.RawMessage(`"route_state"`)
	got, ok = safeLog(row, allowed)
	if !ok || got.Operation != "component_start" || got.Code != "route_state" {
		t.Fatalf("native lifecycle semantics lost: %+v ok=%v", got, ok)
	}
	row["project"] = json.RawMessage(`"escaped-project"`)
	if _, ok := safeLog(row, allowed); ok {
		t.Fatal("provider project escape accepted")
	}
	var nilService *Service
	if _, err := nilService.ListLogs(t.Context(), auth.User{}, "30d", "", "", "", 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unbounded log period err=%v", err)
	}
	if _, err := nilService.Metrics(t.Context(), auth.User{}, "arbitrary.metric", "paperboat-server", "24h"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("arbitrary metric err=%v", err)
	}
	if _, err := nilService.ListLogs(t.Context(), auth.User{}, "24h", "", "", "customer-secret", 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("arbitrary component err=%v", err)
	}
	if got, err := nilService.CorrelateReference(t.Context(), auth.User{}, "bad", "24h", 1); !errors.Is(err, ErrInvalid) || got.SupportReference != "" || len(got.Signals) != 0 {
		t.Fatalf("error leaked partial data: %+v err=%v", got, err)
	}
	metrics := metricAllowlist()
	for _, name := range []string{"paperboat_server_machine_requested_total_snapshot", "paperboat.database.connections", "paperboat_relay_sessions", "paperboat_edge_requests_total_snapshot"} {
		if metrics[name].Aggregation == "" {
			t.Fatalf("producer metric %q missing", name)
		}
	}
	if at, ok := metricLastSeen(1.789832798810751e18); !ok || at.Year() != 2026 {
		t.Fatalf("nanosecond lastSeen=%v ok=%v", at, ok)
	}
	var metric rawMetricDefinition
	if err := json.Unmarshal([]byte(`{"name":"paperboat.operation.count","type":"counter","count":5.0,"lastSeen":1.789832798810751e18}`), &metric); err != nil || metric.Count != 5 {
		t.Fatalf("live metric shape=%+v err=%v", metric, err)
	}
	var traceItems []rawTraceItem
	if err := json.Unmarshal([]byte(`[{"event_id":"abcdefabcdefabcd","event_type":"span","project_id":101,"project_slug":"native","start_timestamp":1767312000,"end_timestamp":1767312001,"errors":[{"event_id":"0123456789abcdef0123456789abcdef","event_type":"error","project_id":101,"project_slug":"native","start_timestamp":1767312000.5}]}]`), &traceItems); err != nil {
		t.Fatal(err)
	}
	if root, ok := safeTraceItem(traceItems[0], map[string]string{"101": "native"}, ""); !ok || root.EventID != "abcdefabcdefabcd" {
		t.Fatalf("live root=%+v ok=%v", root, ok)
	}
	if child, ok := safeTraceItem(traceItems[0].Errors[0], map[string]string{"101": "native"}, "abcdefabcdefabcd"); !ok || !child.Start.Equal(child.End) {
		t.Fatalf("live instant error=%+v ok=%v", child, ok)
	}
	overflow := traceItems[0]
	overflow.Start, overflow.End = 1e11, 1e11+1
	if _, ok := safeTraceItem(overflow, map[string]string{"101": "native"}, ""); ok {
		t.Fatal("overflowing trace timestamp accepted")
	}
	overflow.Start, overflow.End = math.Inf(1), math.Inf(1)
	if _, ok := safeTraceItem(overflow, map[string]string{"101": "native"}, ""); ok {
		t.Fatal("non-finite trace timestamp accepted")
	}
}

func TestOperatorReportingDatabaseAndSentryReads(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	// Keep the pool open until the later fixture cleanup has removed its rows.
	t.Cleanup(func() { _ = store.Close() })
	if err = db.Migrate(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	actorID, ownerID := "usr_reporting_"+suffix, "usr_owner_"+suffix
	ownerID2 := ownerID + "z"
	ref, eventID, issueID := "support_01234567-89ab-4def-8123-456789abcdef", "0123456789abcdef0123456789abcdef", "42"
	traceID := "abcdefabcdefabcdefabcdefabcdefab"
	spanID := "abcdefabcdefabcd"
	_, err = store.Pool().Exec(t.Context(), `INSERT INTO users(id,workos_subject,primary_email,status,role) VALUES($1,$1,$1||'@test','active','support'),($2,$2,$2||'@test','active','user'),($3,$3,$3||'@test','active','user')`, actorID, ownerID, ownerID2)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Pool().Exec(t.Context(), `INSERT INTO support_reports(id,support_reference,owner_id,request_id,component,release,stage,code,created_at,expires_at,sentry_event_id,source_file,source_function,source_line,reporting) VALUES($1,$2,$3,'req','paperboat-server','dev','serve','panic',now(),now()+interval '1 hour',$4,'main.go','serve',10,'unconfirmed')`, "pb-report-"+suffix, ref, ownerID, eventID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Pool().Exec(t.Context(), `INSERT INTO support_reports(id,support_reference,owner_id,request_id,component,release,stage,code,created_at,expires_at,sentry_event_id,source_file,source_function,source_line,reporting) VALUES($1,$2,$3,'req2','paperboat-server','dev','serve','panic',now(),now()+interval '1 hour','','main.go','serve',10,'disabled')`, "pb-report2-"+suffix, ref, ownerID2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := store.Pool().Exec(context.Background(), `DELETE FROM support_reports WHERE support_reference=$1`, ref); err != nil {
			t.Errorf("remove support reference fixture: %v", err)
		}
		// Audit rows are append-only and retain their actors. The acceptance
		// runner owns and drops the isolated database; never bypass that policy.
	})

	traceEscape, revokeOnIssue, partialCapability := false, false, false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/0/organizations/test-org/events/":
			if got := r.URL.Query()["project"]; len(got) != 2 || got[0] != "99" || got[1] != "100" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fields := r.URL.Query()["field"]
			if slices.Contains(fields, "tag[component,string]") || !slices.Contains(fields, "component") || !slices.Contains(fields, "support_reference") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if r.URL.Query().Get("dataset") == "logs" && r.URL.Query().Get("query") == "component:paperboat-daemon" {
				w.Header().Set("Link", `<https://sentry.test/events/?cursor=next>; rel="next"; results="true"`)
				_, _ = w.Write([]byte(`{"data":[{"id":"log1","project":"native","timestamp":"2026-01-02T00:00:00Z","severity":"error","trace":"` + traceID + `","component":"paperboat-daemon","operation":"service_lifecycle","outcome":"failed","code":"start_failed","message":"secret"}]}`))
				return
			}
			if partialCapability && r.URL.Query().Get("dataset") == "logs" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"signal1","project":"native","timestamp":"2026-01-02T00:00:00Z","trace":"` + traceID + `","component":"paperboat-daemon","operation":"service_lifecycle","outcome":"failed","code":"start_failed","support_reference":"` + ref + `","message":"secret"}]}`))
		case "/api/0/organizations/test-org/trace/" + traceID + "/":
			projectID := "100"
			if traceEscape {
				projectID = "999"
			}
			_, _ = w.Write([]byte(`[{"event_id":"` + spanID + `","event_type":"span","project_id":` + projectID + `,"project_slug":"native","op":"service_lifecycle","start_timestamp":1767312000,"end_timestamp":1767312001,"duration":1000,"errors":[{"event_id":"` + eventID + `","event_type":"error","project_id":` + projectID + `,"project_slug":"native","start_timestamp":1767312000.5}]}]`))
		case "/api/0/organizations/test-org/events-timeseries/":
			y := r.URL.Query().Get("yAxis")
			if strings.Contains(y, "paperboat.runtime.goroutines") && r.URL.Query().Get("query") != "component:paperboat-server region:mumbai instance:slot-01" {
				t.Errorf("unscoped fleet query: %s", r.URL.Query().Get("query"))
				w.WriteHeader(400)
				return
			}
			body, _ := json.Marshal(map[string]any{"timeSeries": []any{map[string]any{"values": []any{
				map[string]any{"timestamp": int64(1767312000000), "value": 3, "sampleCount": 3},
				map[string]any{"timestamp": int64(1767312300000), "value": 0, "sampleCount": 0},
				map[string]any{"timestamp": int64(1767312600000), "value": 0, "sampleCount": 1},
				map[string]any{"timestamp": int64(1767312900000), "value": nil, "sampleCount": 0},
			}, "yAxis": y, "groupBy": []any{}}}})
			_, _ = w.Write(body)
		case "/api/0/organizations/test-org/trace-items/metrics/":
			if r.URL.Query().Has("query") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`[{"name":"paperboat.operation.count","type":"counter","unit":null,"count":5.0,"lastSeen":1789832798810751000},{"name":"paperboat.secret.name","type":"gauge","count":1.0,"lastSeen":1789832798810751000}]`))
		case "/api/0/organizations/test-org/issues/":
			if r.URL.Query().Has("statsPeriod") || r.URL.Query().Get("collapse") != "stats" {
				t.Error("issue query must omit empty periods and collapse unneeded statistics")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if revokeOnIssue {
				_, _ = store.Pool().Exec(r.Context(), `UPDATE users SET role='user' WHERE id=$1`, actorID)
			}
			if got := r.URL.Query()["project"]; len(got) != 2 || got[0] != "99" || got[1] != "100" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`[{"id":"42","shortId":"PB-1","title":"panic","status":"unresolved","count":"2","firstSeen":"2026-01-01T00:00:00Z","lastSeen":"2026-01-02T00:00:00Z","project":{"id":"99","slug":"test-project"}}]`))
		case "/api/0/projects/test-org/test-project/":
			_, _ = w.Write([]byte(`{"id":"99","slug":"test-project"}`))
		case "/api/0/projects/test-org/native/":
			_, _ = w.Write([]byte(`{"id":"100","slug":"native"}`))
		case "/api/0/projects/test-org/test-project/events/" + eventID + "/":
			_, _ = w.Write([]byte(`{"eventID":"` + eventID + `","groupID":"42","dateCreated":"2026-01-02T00:00:00Z","title":"panic","tags":[{"key":"support_reference","value":"` + ref + `"},{"key":"request_body","value":"must-not-return"}]}`))
		case "/api/0/organizations/test-org/issues/42/":
			_, _ = w.Write([]byte(`{"id":"42","title":"panic","status":"unresolved","count":"2","firstSeen":"2026-01-01T00:00:00Z","lastSeen":"2026-01-02T00:00:00Z","project":{"id":"99","slug":"test-project"},"latestEvent":{"request":{"data":"must-not-return"}}}`))
		case "/api/0/organizations/test-org/issues/42/events/":
			_, _ = w.Write([]byte(`[{"eventID":"` + eventID + `","dateCreated":"2026-01-02T00:00:00Z","title":"panic","tags":[{"key":"support_reference","value":"` + ref + `"}]}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	svc, err := New(store, nil, Config{BaseURL: server.URL, Organization: "test-org", Projects: []string{"test-project", "native"}, Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	svc.client = server.Client()
	svc.client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	actor := auth.User{ID: actorID, Role: auth.RoleAdmin}
	local, err := svc.ListLocalReferences(t.Context(), actor, "", 1)
	if err != nil || len(local.References) != 1 || local.References[0].Reference != ref || local.NextAfter == "" {
		t.Fatalf("local=%+v err=%v", local, err)
	}
	local2, err := svc.ListLocalReferences(t.Context(), actor, local.NextAfter, 1)
	if err != nil || len(local2.References) != 1 || local2.References[0].OwnerID != ownerID2 {
		t.Fatalf("local2=%+v err=%v", local2, err)
	}
	issues, err := svc.ListIssues(t.Context(), actor, "", 10)
	if err != nil || len(issues.Issues) != 1 || issues.Issues[0].ID != issueID {
		t.Fatalf("issues=%+v err=%v", issues, err)
	}
	detail, err := svc.GetIssue(t.Context(), actor, issueID, 1)
	if err != nil || len(detail.Events) != 1 || detail.Events[0].SupportReference != ref {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
	resolved, err := svc.ResolveReference(t.Context(), actor, ref, ownerID)
	if err != nil || resolved.Issue.ID != issueID || resolved.Event.SupportReference != ref {
		t.Fatalf("resolved=%+v err=%v", resolved, err)
	}
	b, _ := json.Marshal(resolved)
	if strings.Contains(string(b), "must-not-return") {
		t.Fatalf("unsafe response: %s", b)
	}
	logs, err := svc.ListLogs(t.Context(), actor, "24h", "", "", "paperboat-daemon", 10)
	if err != nil || len(logs.Logs) != 1 || logs.Logs[0].Operation != "service_lifecycle" || logs.NextCursor != "next" {
		t.Fatalf("logs=%+v err=%v", logs, err)
	}
	correlated, err := svc.CorrelateReference(t.Context(), actor, ref, "24h", 10)
	if err != nil || len(correlated.Signals) != 3 || correlated.Signals[0].TraceID != traceID {
		t.Fatalf("correlated=%+v err=%v", correlated, err)
	}
	partialCapability = true
	partial, err := svc.CorrelateReference(t.Context(), actor, ref, "24h", 10)
	partialCapability = false
	if err != nil || len(partial.Signals) != 2 || partial.Datasets["logs"] != "unsupported" || partial.Datasets["errors"] != "available" {
		t.Fatalf("partial=%+v err=%v", partial, err)
	}
	trace, err := svc.GetTrace(t.Context(), actor, traceID, "24h")
	if err != nil || len(trace.Items) != 2 || trace.Items[0].EventID != spanID || trace.Items[1].EventID != eventID || !trace.Items[1].End.Equal(trace.Items[1].Start) {
		t.Fatalf("trace=%+v err=%v", trace, err)
	}
	traceEscape = true
	if _, err = svc.GetTrace(t.Context(), actor, traceID, "24h"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("trace project escape err=%v", err)
	}
	traceEscape = false
	metrics, err := svc.Metrics(t.Context(), actor, "paperboat.operation.count", "paperboat-daemon", "24h")
	if err != nil || len(metrics.Points) != 2 || metrics.Points[0].Value != 3 || metrics.Points[1].Value != 0 || metrics.Points[1].Timestamp.UnixMilli() != 1767312600000 {
		t.Fatalf("metrics=%+v err=%v", metrics, err)
	}
	scoped, err := svc.Metrics(t.Context(), actor, "paperboat.runtime.goroutines", "paperboat-server", "24h", FleetFilter{Region: "mumbai", Instance: "slot-01"})
	if err != nil || scoped.Scope != "deployment_slot" || scoped.Semantics != "interval_slot_maximum" || scoped.Freshness != "unknown" || scoped.LastObservedBucket == nil || scoped.LastObservedBucket.UnixMilli() != 1767312600000 || len(scoped.Points) != 2 {
		t.Fatalf("scoped=%+v err=%v", scoped, err)
	}
	catalog, err := svc.MetricCatalog(t.Context(), actor, "24h")
	if err != nil || len(catalog.Metrics) != 1 || catalog.Metrics[0].Name != "paperboat.operation.count" {
		t.Fatalf("catalog=%+v err=%v", catalog, err)
	}
	b, _ = json.Marshal([]any{logs, correlated, trace, catalog})
	if strings.Contains(string(b), "secret") {
		t.Fatalf("signal secret leaked: %s", b)
	}
	var audits int
	if err = store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE actor_user_id=$1 AND event_type='operator_reporting.read'`, actorID).Scan(&audits); err != nil || audits != 12 {
		t.Fatalf("audits=%d err=%v", audits, err)
	}
	revokeOnIssue = true
	if _, err = svc.ListIssues(t.Context(), actor, "", 10); !errors.Is(err, ErrForbidden) {
		t.Fatalf("revoked between fetch and audit err=%v", err)
	}
	if err = store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE actor_user_id=$1 AND event_type='operator_reporting.read'`, actorID).Scan(&audits); err != nil || audits != 12 {
		t.Fatalf("revoked disclosure wrote audit: %d err=%v", audits, err)
	}
}
