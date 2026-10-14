package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/billing"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/operator"
	"github.com/pinksaucepasta/paperboat-server/internal/operatoractions"
)

type operatorAuditFake struct {
	events []audit.Event
	failAt int
}

func (a *operatorAuditFake) Write(_ context.Context, e audit.Event) error {
	a.events = append(a.events, e)
	if len(a.events) == a.failAt {
		return errors.New("offline")
	}
	return nil
}
func operatorTestRequest(method, pattern string, role auth.Role, at time.Time) *http.Request {
	r := httptest.NewRequest(method, "/v1/admin/test", nil)
	r.Pattern = pattern
	r.SetPathValue("operation_id", "op_test")
	r.SetPathValue("account_id", "usr_target")
	r.Header.Set("Operator-Target", "op_test")
	r.Header.Set("Operator-Reason", "ticket_123")
	r.Header.Set("Idempotency-Key", "retry_123")
	return r.WithContext(context.WithValue(r.Context(), authContextKey{}, principal{User: auth.User{ID: "usr_staff", Role: role}, Session: auth.Session{ID: "ses_staff", OperatorAuthenticatedAt: at}}))
}
func TestOperatorCapability(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name, method, route string
		role                auth.Role
		at                  time.Time
		want                int
	}{
		{"support_read", "GET", "GET /v1/admin/accounts/{account_id}", auth.RoleSupport, now, 200},
		{"billing_read", "GET", "GET /v1/admin/accounts/{account_id}", auth.RoleBilling, now, 200},
		{"operations_no_accounts", "GET", "GET /v1/admin/accounts/{account_id}", auth.RoleOperations, now, 403},
		{"user_denied", "GET", "GET /v1/admin/accounts/{account_id}", auth.RoleUser, now, 403},
		{"worker_denied", "GET", "GET /v1/admin/accounts/{account_id}", auth.RoleSystem, now, 403},
		{"support_no_mutation", "POST", "POST /v1/admin/control-operations/{operation_id}/recover", auth.RoleSupport, now, 403},
		{"ops_recover", "POST", "POST /v1/admin/control-operations/{operation_id}/recover", auth.RoleOperations, now, 200},
		{"billing_no_control", "POST", "POST /v1/admin/control-operations/{operation_id}/recover", auth.RoleBilling, now, 403},
		{"admin_read", "GET", "GET /v1/admin/accounts/{account_id}", auth.RoleAdmin, now, 200},
		{"admin_write", "POST", "POST /v1/admin/control-operations/{operation_id}/recover", auth.RoleAdmin, now, 200},
		{"unknown_denied", "GET", "GET /v1/admin/new-route", auth.RoleAdmin, now, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &operatorAuditFake{}
			called := false
			w := httptest.NewRecorder()
			operatorGuard(a, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.Write([]byte("safe")) })).ServeHTTP(w, operatorTestRequest(tc.method, tc.route, tc.role, tc.at))
			if w.Code != tc.want || called != (tc.want == 200) {
				t.Fatalf("status %d called %t", w.Code, called)
			}
			if w.Header().Get("Cache-Control") != "no-store" || len(a.events) == 0 {
				t.Fatal("uncached audit required")
			}
		})
	}
}

func TestOperatorReadErrorMapsOnlyPureExpectedCauses(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "forbidden", err: operator.ErrForbidden, status: http.StatusForbidden, code: "forbidden"},
		{name: "repeated pure forbidden", err: errors.Join(operator.ErrForbidden, operator.ErrForbidden), status: http.StatusForbidden, code: "forbidden"},
		{name: "invalid", err: operator.ErrInvalid, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "not found", err: operator.ErrNotFound, status: http.StatusNotFound, code: "not_found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req, logs, _ := diagnosticRequest("/v1/admin/accounts/usr_target")
			response := httptest.NewRecorder()
			writeOperatorReadError(response, req, test.err)
			if response.Code != test.status {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), test.status)
			}
			var body ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != test.code {
				t.Fatalf("error code=%q, want %q", body.Error.Code, test.code)
			}
			if logs.Len() != 0 {
				t.Fatalf("pure expected response emitted unexpected failure log: %s", logs.String())
			}
		})
	}
}

func TestOperatorReadErrorReportsMixedFailuresOnceAndSafely(t *testing.T) {
	expected := []struct {
		name string
		err  error
	}{
		{name: "forbidden", err: operator.ErrForbidden},
		{name: "invalid", err: operator.ErrInvalid},
		{name: "not found", err: operator.ErrNotFound},
	}
	for _, item := range expected {
		t.Run(item.name, func(t *testing.T) {
			req, logs, reference := diagnosticRequest("/v1/admin/accounts/usr_target")
			private := errors.New("OPERATOR_PRIVATE_DATABASE_DETAIL")
			err := errors.Join(item.err, syscall.EIO, private)
			response := httptest.NewRecorder()
			writeOperatorReadError(response, req, err)
			if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), private.Error()) {
				t.Fatalf("mixed read failure response=%d %s", response.Code, response.Body.String())
			}
			var body ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != "operator_unavailable" || body.Error.SupportReference != reference || body.Error.CorrelationID != "correlation_fault_01" {
				t.Fatalf("response lost safe code/reference: %+v", body.Error)
			}
			var event map[string]any
			if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
				t.Fatalf("expected exactly one structured failure: %v; logs=%s", err, logs.String())
			}
			if event["stage"] != "http_handler" || event["code"] != "operator_unavailable" || event["cause"] != "system_error" || event["errno"] != float64(syscall.EIO) || event["http_status"] != float64(http.StatusServiceUnavailable) || event["support_reference"] != reference || event["correlation_id"] != "correlation_fault_01" || event["request_id"] != "request_fault_01" {
				t.Fatalf("failure record lost operational cause or identity: %v", event)
			}
			if strings.Contains(logs.String(), private.Error()) {
				t.Fatal("failure log exposed private database detail")
			}
		})
	}
}

func TestOperatorReadClosedPoolUsesUnavailableFinalOwner(t *testing.T) {
	dsn := "postgres://127.0.0.1:1/paperboat_operator_closedpool_test?sslmode=disable"
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal("test database DSN is not isolated")
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("open isolated test database: %T", err)
	}
	defer store.Close()
	service := operator.NewService(store)
	if err := store.Close(); err != nil {
		t.Fatalf("close isolated test database: %T", err)
	}

	req, logs, reference := diagnosticRequest("/v1/admin/accounts/usr_target")
	req.SetPathValue("account_id", "usr_target")
	ctx := context.WithValue(req.Context(), authContextKey{}, principal{User: auth.User{ID: "usr_operator", Role: auth.RoleAdmin}})
	response := httptest.NewRecorder()
	operatorRead(service, "account", "account_id").ServeHTTP(response, req.WithContext(ctx))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed database became an expected denial: status=%d body=%s", response.Code, response.Body.String())
	}
	var body ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "operator_unavailable" || body.Error.SupportReference != reference || body.Error.CorrelationID != "correlation_fault_01" {
		t.Fatalf("closed-pool response lost safe fault identity: %+v", body.Error)
	}
	var event map[string]any
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatalf("expected exactly one safe structured failure: %v; logs=%s", err, logs.String())
	}
	if event["stage"] != "http_handler" || event["code"] != "operator_unavailable" || event["http_status"] != float64(http.StatusServiceUnavailable) || event["support_reference"] != reference || event["correlation_id"] != "correlation_fault_01" || event["request_id"] != "request_fault_01" {
		t.Fatalf("closed-pool report lost final-owner identity: %v", event)
	}
	if cause, ok := event["cause"].(string); !ok || cause == "" || event["error_type"] == "" {
		t.Fatalf("closed-pool report lost bounded cause metadata: %v", event)
	}
}

func TestOperatorMutationErrorReportsMixedFailureOnceAndSafely(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
	}{
		{name: "forbidden", err: operatoractions.ErrForbidden, status: http.StatusForbidden},
		{name: "invalid", err: operatoractions.ErrInvalid, status: http.StatusBadRequest},
		{name: "not found", err: operatoractions.ErrNotFound, status: http.StatusNotFound},
		{name: "conflict", err: operatoractions.ErrConflict, status: http.StatusConflict},
		{name: "billing forbidden", err: billing.ErrOperatorBillingForbidden, status: http.StatusForbidden},
		{name: "billing invalid", err: billing.ErrOperatorBillingInvalid, status: http.StatusBadRequest},
		{name: "billing not found", err: billing.ErrOperatorBillingNotFound, status: http.StatusNotFound},
		{name: "billing conflict", err: billing.ErrOperatorBillingConflict, status: http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			req, logs, _ := diagnosticRequest("/v1/admin/relays/node_test/drain")
			response := httptest.NewRecorder()
			if !operatorMutationError(response, req, test.err) {
				t.Fatal("expected the mutation error owner to handle the error")
			}
			if response.Code != test.status {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), test.status)
			}
			if logs.Len() != 0 {
				t.Fatalf("pure expected response emitted an operational fault: %s", logs.String())
			}
		})
	}

	req, logs, reference := diagnosticRequest("/v1/admin/relays/node_test/drain")
	private := errors.New("OPERATOR_ACTION_PRIVATE_DETAIL")
	operational := errors.Join(operatoractions.ErrConflict, syscall.EIO, private)
	response := httptest.NewRecorder()
	if !operatorMutationError(response, req, operational) {
		t.Fatal("expected the mutation error owner to handle the error")
	}
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), private.Error()) {
		t.Fatalf("mixed failure response=%d body=%s", response.Code, response.Body.String())
	}
	var body ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "operator_unavailable" || body.Error.SupportReference != reference || body.Error.CorrelationID != "correlation_fault_01" {
		t.Fatalf("response lost safe failure identity: %+v", body.Error)
	}
	var event map[string]any
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatalf("expected exactly one structured failure: %v; logs=%s", err, logs.String())
	}
	if event["stage"] != "http_handler" || event["code"] != "operator_unavailable" || event["cause"] != "system_error" || event["errno"] != float64(syscall.EIO) || event["http_status"] != float64(http.StatusServiceUnavailable) || event["support_reference"] != reference || event["correlation_id"] != "correlation_fault_01" || event["request_id"] != "request_fault_01" {
		t.Fatalf("failure report lost its original cause or identity: %v", event)
	}
	if strings.Contains(logs.String(), private.Error()) {
		t.Fatal("operational failure log exposed private detail")
	}
}

func TestOperatorConfirmationAndAuditFailure(t *testing.T) {
	for _, tc := range []struct {
		name          string
		fail          int
		header, value string
		want          int
		called        bool
	}{
		{"success", 0, "", "", 200, true}, {"intent_failure", 1, "", "", 503, false}, {"outcome_failure", 2, "", "", 503, true},
		{"wrong_target", 0, "Operator-Target", "op_other", 400, false}, {"missing_reason", 0, "Operator-Reason", "", 400, false}, {"missing_key", 0, "Idempotency-Key", "", 400, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &operatorAuditFake{failAt: tc.fail}
			called := false
			r := operatorTestRequest("POST", "POST /v1/admin/control-operations/{operation_id}/recover", auth.RoleAdmin, time.Now())
			if tc.header != "" {
				r.Header.Set(tc.header, tc.value)
			}
			w := httptest.NewRecorder()
			operatorGuard(a, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if len(a.events) != 1 {
					t.Fatal("missing intent before mutation")
				}
				w.Header().Set("Private-Test", "sensitive")
				w.Write([]byte("private result"))
			})).ServeHTTP(w, r)
			if w.Code != tc.want || called != tc.called {
				t.Fatalf("status %d called %t", w.Code, called)
			}
			if tc.want != 200 && (strings.Contains(w.Body.String(), "private result") || w.Header().Get("Private-Test") != "") {
				t.Fatal("unaudited data disclosed")
			}
		})
	}
}
func TestOperatorNilAuditBearerAndBoundedResponse(t *testing.T) {
	r := operatorTestRequest("GET", "GET /v1/admin/accounts/{account_id}", auth.RoleAdmin, time.Now())
	w := httptest.NewRecorder()
	operatorGuard(nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("nil audit allowed") })).ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	p, _ := principalFromContext(r.Context())
	p.Client = &auth.ClientPrincipal{}
	r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, p))
	w = httptest.NewRecorder()
	operatorGuard(&operatorAuditFake{}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("bearer allowed") })).ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	r = operatorTestRequest("GET", "GET /v1/admin/accounts/{account_id}", auth.RoleAdmin, time.Now())
	w = httptest.NewRecorder()
	operatorGuard(&operatorAuditFake{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", operatorResponseLimit+1)))
	})).ServeHTTP(w, r)
	if w.Code != 503 || w.Body.Len() > 1024 {
		t.Fatal("unbounded disclosure")
	}
}
func TestEveryOperatorRouteHasPolicyAndGuard(t *testing.T) {
	source, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatal(err)
	}
	routes := regexp.MustCompile(`mux.Handle\("((?:GET|POST|DELETE|PUT|PATCH) /v1/admin/[^\"]+)", ([^\n]+)`).FindAllStringSubmatch(string(source), -1)
	if len(routes) < 12 {
		t.Fatal("route inventory unexpectedly empty")
	}
	for _, m := range routes {
		if _, ok := operatorPolicies[m[1]]; !ok {
			t.Errorf("route lacks policy: %s", m[1])
		}
		if !strings.HasPrefix(m[2], "operatorRoute(opts,") {
			t.Errorf("route bypasses staff guard: %s", m[1])
		}
	}
}

func TestOperatorOpenAPIRequiresSeparateOperatorSurface(t *testing.T) {
	raw, err := os.ReadFile("../../docs/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	type operationDocument struct {
		Security   []map[string]any `json:"security"`
		Parameters []map[string]any `json:"parameters"`
		Capability string           `json:"x-operator-capability"`
	}
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}

	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for route, policy := range operatorPolicies {
		method, path, _ := strings.Cut(route, " ")
		rawOperation, ok := doc.Paths[path][strings.ToLower(method)]
		var op operationDocument
		if ok {
			if err := json.Unmarshal(rawOperation, &op); err != nil {
				t.Fatal(err)
			}
		}
		if !ok {
			t.Errorf("undocumented route %s", route)
			continue
		}
		if op.Capability != policy.capability || len(op.Security) != 1 {
			t.Errorf("missing capability or ambiguous authentication %s", route)
			continue
		}
		if _, ok = op.Security[0]["operatorCookie"]; !ok {
			t.Errorf("missing operator authentication %s", route)
		}
		if _, ok = op.Security[0]["cookieSession"]; ok {
			t.Errorf("customer session accepted %s", route)
		}
		if method != "GET" {
			if _, ok = op.Security[0]["csrfHeader"]; !ok {
				t.Errorf("missing conjunctive CSRF %s", route)
			}
			for _, name := range []string{"Operator-Target", "Operator-Reason"} {
				found := false
				for _, param := range op.Parameters {
					found = found || (param["name"] == name && param["required"] == true)
				}
				if !found {
					t.Errorf("missing %s on %s", name, route)
				}
			}
		}
	}
}
