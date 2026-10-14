package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/billing"
	"github.com/pinksaucepasta/paperboat-server/internal/operator"
	"github.com/pinksaucepasta/paperboat-server/internal/operatoractions"
	"github.com/pinksaucepasta/paperboat-server/internal/telemetry"
)

// Staff authority is independent of workspace/team membership. Unknown routes deny.
type operatorPolicy struct{ capability, parameter, target string }

var operatorPolicies = map[string]operatorPolicy{
	"GET /v1/admin/client-update-policy":                       {"admin", "", "client-update-policy"},
	"PUT /v1/admin/client-update-policy":                       {"admin", "", "client-update-policy"},
	"POST /v1/admin/selfhost/enrollments":                      {"admin", "body:address", "selfhost"},
	"GET /v1/admin/selfhost/installations":                     {"operations", "", "selfhost"},
	"GET /v1/admin/selfhost/installations/{installationID}":    {"operations", "installationID", ""},
	"DELETE /v1/admin/selfhost/installations/{installationID}": {"admin", "installationID", "selfhost"},
	"GET /v1/admin/regions":                                    {"operations", "", "regions"},
	"GET /v1/admin/nodes":                                      {"operations", "", "nodes"},
	"POST /v1/admin/regions/{region_id}/create":                {"admin", "region_id", ""},
	"POST /v1/admin/regions/{region_id}/update":                {"admin", "region_id", ""},
	"POST /v1/admin/regions/{region_id}/enable":                {"admin", "region_id", ""},
	"POST /v1/admin/regions/{region_id}/disable":               {"admin", "region_id", ""},
	"POST /v1/admin/nodes/{node_id}/enroll":                    {"admin", "node_id", ""},
	"POST /v1/admin/nodes/{node_id}/update":                    {"admin", "node_id", ""},
	"POST /v1/admin/nodes/{node_id}/drain":                     {"admin", "node_id", ""},
	"POST /v1/admin/nodes/{node_id}/resume":                    {"admin", "node_id", ""},
	"POST /v1/admin/nodes/{node_id}/retire":                    {"admin", "node_id", ""},
	"POST /v1/admin/nodes/{node_id}/rotate":                    {"admin", "node_id", ""},
	"POST /v1/admin/nodes/{node_id}/verify-ingress":            {"admin", "node_id", ""},

	"GET /v1/admin/backups":                                                         {"operations", "", "backups"},
	"GET /v1/admin/backups/runs":                                                    {"operations", "", "backup-runs"},
	"GET /v1/admin/support":                                                         {"support", "", "support-references"},
	"GET /v1/admin/sentry/logs":                                                     {"operations", "", "sentry-logs"},
	"GET /v1/admin/sentry/traces/{trace_id}":                                        {"operations", "trace_id", ""},
	"GET /v1/admin/sentry/references/{reference}/signals":                           {"operations", "reference", ""},
	"GET /v1/admin/sentry/metrics":                                                  {"operations", "", "sentry-metrics"},
	"GET /v1/admin/sentry/metrics/catalog":                                          {"operations", "", "sentry-metric-catalog"},
	"GET /v1/admin/sentry/monitors":                                                 {"operations", "", "sentry-monitors"},
	"GET /v1/admin/sentry/monitors/{slug}/checkins":                                 {"operations", "slug", ""},
	"GET /v1/admin/sentry/outcomes":                                                 {"operations", "", "sentry-outcomes"},
	"GET /v1/admin/sentry/detectors":                                                {"operations", "", "sentry-detectors"},
	"GET /v1/admin/sentry/issues":                                                   {"support", "", "sentry-issues"},
	"GET /v1/admin/sentry/issues/{issue_id}":                                        {"support", "issue_id", ""},
	"GET /v1/admin/sentry/references/{reference}":                                   {"support", "reference", ""},
	"GET /v1/admin/tunnels/{tunnel_id}/status":                                      {"operations", "tunnel_id", ""},
	"GET /v1/admin/status":                                                          {"operations", "", "status"},
	"GET /v1/admin/overview":                                                        {"admin", "", "overview"},
	"GET /v1/admin/accounts":                                                        {"support", "", "accounts"},
	"GET /v1/admin/teams":                                                           {"support", "", "teams"},
	"GET /v1/admin/billing/workspaces":                                              {"support", "", "workspaces"},
	"GET /v1/admin/billing/workspace-operations":                                    {"billing", "", "billing-operations"},
	"GET /v1/admin/control-operations":                                              {"operations", "", "control-operations"},
	"GET /v1/admin/billing/products":                                                {"billing", "", "billing-products"},
	"PUT /v1/admin/billing/products/{code}":                                         {"admin", "code", ""},
	"GET /v1/admin/config/billing":                                                  {"billing", "", "billing-policy"},
	"PUT /v1/admin/config/billing":                                                  {"admin", "", "billing-policy"},
	"POST /v1/admin/accounts/{account_id}/ban":                                      {"admin", "account_id", ""},
	"POST /v1/admin/accounts/{account_id}/unban":                                    {"admin", "account_id", ""},
	"POST /v1/admin/teams/{team_id}/ban":                                            {"admin", "team_id", ""},
	"POST /v1/admin/teams/{team_id}/unban":                                          {"admin", "team_id", ""},
	"POST /v1/admin/relays/{node_id}/drain":                                         {"operations", "node_id", ""},
	"POST /v1/admin/relays/{node_id}/resume":                                        {"operations", "node_id", ""},
	"POST /v1/admin/tunnels/{tunnel_id}/disable":                                    {"operations", "tunnel_id", ""},
	"POST /v1/admin/tunnels/{tunnel_id}/enable":                                     {"operations", "tunnel_id", ""},
	"GET /v1/admin/accounts/{account_id}":                                           {"support", "account_id", ""},
	"GET /v1/admin/teams/{team_id}":                                                 {"support", "team_id", ""},
	"GET /v1/admin/billing/workspaces/{workspace_id}":                               {"support", "workspace_id", ""},
	"GET /v1/admin/infrastructure":                                                  {"operations", "", "infrastructure"},
	"GET /v1/admin/reporting":                                                       {"operations", "", "reporting"},
	"GET /v1/admin/audit/{resource_id}":                                             {"admin", "resource_id", ""},
	"GET /v1/admin/control-operations/{operation_id}":                               {"operations", "operation_id", ""},
	"GET /v1/admin/billing/workspace-operations/{operation_id}":                     {"billing", "operation_id", ""},
	"GET /v1/admin/support/{reference}":                                             {"support", "reference", ""},
	"GET /v1/admin/release-authority/bundles":                                       {"admin", "", "release-authority"},
	"POST /v1/admin/release-authority/bundles":                                      {"admin", "body:release_id", ""},
	"GET /v1/admin/release-authority/requests":                                      {"admin", "", "release-authority"},
	"POST /v1/admin/release-authority/requests":                                     {"admin", "body:release_id", ""},
	"POST /v1/admin/edge/usage-keys":                                                {"admin", "body:key_id", ""},
	"POST /v1/admin/edge/usage-keys/{key_id}/revoke":                                {"admin", "key_id", ""},
	"POST /v1/admin/mint/signing-keys/{key_id}/revoke":                              {"admin", "key_id", ""},
	"POST /v1/admin/control-operations/{operation_id}/recover":                      {"operations", "operation_id", ""},
	"POST /v1/admin/billing/workspace-operations/{operation_id}/retire":             {"billing", "operation_id", ""},
	"POST /v1/admin/billing/workspace-operations/{operation_id}/reconcile-checkout": {"billing", "operation_id", ""},
	"POST /v1/admin/billing/workspace-operations/{operation_id}/supersede-portal":   {"billing", "operation_id", ""},
}

const operatorResponseLimit = 2 << 20

type operatorAudit interface {
	Write(context.Context, audit.Event) error
}

func operatorAllowed(role auth.Role, capability string) bool {
	if role == auth.RoleAdmin {
		return true
	}
	switch capability {
	case "support":
		return role == auth.RoleSupport || role == auth.RoleBilling
	case "billing":
		return role == auth.RoleBilling
	case "operations":
		return role == auth.RoleOperations
	}
	return false
}
func operatorToken(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c == ':') {
			return false
		}
	}
	return true
}
func operatorRoute(opts Options, next http.Handler) http.Handler {
	return requireOperator(opts, operatorGuard(opts.OperatorAudit, next))
}
func operatorGuard(writer operatorAudit, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := principalFromContext(r.Context())
		policy, known := operatorPolicies[r.Pattern]
		target := policy.target
		if policy.parameter != "" {
			if field, bodyTarget := strings.CutPrefix(policy.parameter, "body:"); bodyTarget {
				raw, err := io.ReadAll(io.LimitReader(r.Body, (64<<10)+1))
				r.Body = io.NopCloser(bytes.NewReader(raw))
				var fields map[string]json.RawMessage
				if err == nil && len(raw) <= 64<<10 && json.Unmarshal(raw, &fields) == nil {
					_ = json.Unmarshal(fields[field], &target)
				}
			} else {
				target = r.PathValue(policy.parameter)
			}
		}
		if !operatorToken(target) {
			target = "invalid"
		}
		record := func(ctx context.Context, outcome string, status int) error {
			if writer == nil {
				return errors.New("operator audit unavailable")
			}
			metadata := map[string]any{"action": r.Pattern, "outcome": outcome, "http_status": status}
			if unsafeMethod(r.Method) && operatorToken(r.Header.Get("Operator-Reason")) {
				metadata["evidence_reference"] = r.Header.Get("Operator-Reason")
			}
			if k := r.Header.Get("Idempotency-Key"); operatorToken(k) {
				sum := sha256.Sum256([]byte(k))
				metadata["idempotency_digest"] = hex.EncodeToString(sum[:])
			}
			return writer.Write(ctx, audit.Event{ActorUserID: p.User.ID, ActorType: audit.ActorAdmin, EventType: "operator.request", ResourceType: "operator_target", ResourceID: target, Metadata: metadata})
		}
		fail := func(status int, code, msg string) {
			var auditErr error
			if ok {
				if auditErr = record(r.Context(), code, status); auditErr != nil {
					status = 503
					code = "operator_audit_unavailable"
					msg = "Operator audit is unavailable. Try again later."
				}
			}
			writeError(w, r, status, code, msg, auditErr)
		}
		if !ok || p.Client != nil || !known || !operatorAllowed(p.User.Role, policy.capability) {
			fail(403, "forbidden", "This staff role cannot perform this action.")
			return
		}
		if target == "invalid" {
			fail(400, "invalid_target", "A valid explicit target is required.")
			return
		}
		if unsafeMethod(r.Method) && (r.Header.Get("Operator-Target") != target || !operatorToken(r.Header.Get("Operator-Reason")) || (!operatorToken(r.Header.Get("Idempotency-Key")) || len(r.Header.Get("Idempotency-Key")) < 8)) {
			fail(400, "operator_confirmation_required", "Confirm Operator-Target and provide an opaque Operator-Reason reference and Idempotency-Key.")
			return
		}
		if err := record(r.Context(), "started", 0); err != nil {
			writeError(w, r, 503, "operator_audit_unavailable", "Operator audit is unavailable. Try again later.", err)
			return
		}
		// Hold the response until outcome auditing succeeds. Services retain their own
		// transactional mutation audits; an interrupted request remains visibly started.
		response := &operatorResponse{header: make(http.Header)}
		defer func() {
			if v := recover(); v != nil {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
				defer cancel()
				_ = record(ctx, "panic", 500)
				panic(v)
			}
		}()
		next.ServeHTTP(response, r)
		status := response.status
		if status == 0 {
			status = 200
		}
		outcome := "completed"
		if status >= 400 {
			outcome = "rejected"
		}
		if response.overflow {
			outcome = "response_limit"
			status = 503
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
		defer cancel()
		auditErr := record(ctx, outcome, status)
		if auditErr != nil || response.overflow {
			writeError(w, r, 503, "operator_result_unavailable", "The result could not be returned. Check the target state and retry mutations with the same Idempotency-Key.", auditErr)
			return
		}
		for k, vs := range response.header {
			w.Header()[k] = vs
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_, _ = w.Write(response.body.Bytes())
	})
}

type operatorResponse struct {
	header   http.Header
	body     bytes.Buffer
	status   int
	overflow bool
}

func (w *operatorResponse) Header() http.Header { return w.header }
func (w *operatorResponse) WriteHeader(s int) {
	if w.status == 0 {
		w.status = s
	}
}
func (w *operatorResponse) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	if len(b) > operatorResponseLimit-w.body.Len() {
		w.overflow = true
		return 0, io.ErrShortBuffer
	}
	return w.body.Write(b)
}

func registerOperatorRoutes(mux *http.ServeMux, opts Options) {
	registerBackupReportRoute(mux, opts)
	if opts.OperatorAuth == nil {
		return
	}
	registerOperatorMutations(mux, opts)
	registerOperatorDiagnostics(mux, opts)
	registerOperatorInfrastructure(mux, opts)
	if opts.Operator == nil {
		return
	}
	for _, route := range []struct{ pattern, kind, param string }{
		{"GET /v1/admin/tunnels/{tunnel_id}/status", "tunnel_status", "tunnel_id"},
		{"GET /v1/admin/overview", "overview", ""},
		{"GET /v1/admin/accounts", "accounts", ""},
		{"GET /v1/admin/teams", "teams", ""},
		{"GET /v1/admin/billing/workspaces", "workspaces", ""},
		{"GET /v1/admin/billing/workspace-operations", "billing_operations", ""},
		{"GET /v1/admin/control-operations", "control_operations", ""},
		{"GET /v1/admin/accounts/{account_id}", "account", "account_id"},
		{"GET /v1/admin/teams/{team_id}", "team", "team_id"},
		{"GET /v1/admin/billing/workspaces/{workspace_id}", "billing", "workspace_id"},
		{"GET /v1/admin/infrastructure", "infrastructure", ""},
		{"GET /v1/admin/control-operations/{operation_id}", "control_operation", "operation_id"},
		{"GET /v1/admin/billing/workspace-operations/{operation_id}", "billing_operation", "operation_id"},
		{"GET /v1/admin/audit/{resource_id}", "audit", "resource_id"},
	} {
		mux.Handle(route.pattern, operatorRoute(opts, operatorRead(opts.Operator, route.kind, route.param)))
	}
	mux.Handle("GET /v1/admin/reporting", operatorRoute(opts, operatorReporting(opts)))
	mux.Handle("GET /v1/admin/status", operatorRoute(opts, operatorStatus(opts)))
}
func operatorRead(service *operator.Service, kind, param string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := principalFromContext(r.Context())
		limit := 50
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > 100 {
				writeError(w, r, 400, "invalid_request", "Limit must be between 1 and 100.")
				return
			}
			limit = n
		}
		target := r.PathValue(param)
		if kind == "accounts" {
			target = r.URL.Query().Get("email")
		}
		if kind == "workspaces" || kind == "billing_operations" || kind == "control_operations" {
			target = r.URL.Query().Get("state")
		}
		if kind == "infrastructure" {
			target = r.URL.Query().Get("kind")
			if target == "" {
				target = "host"
			}
		}
		if kind == "audit" {
			target = r.URL.Query().Get("resource_type") + ":" + target
		}
		result, err := service.Read(r.Context(), p.User, kind, target, r.URL.Query().Get("after"), limit)
		if err != nil {
			writeOperatorReadError(w, r, err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: result})
	})
}

func writeOperatorReadError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case expectedHTTPErrorOnly(err, operator.ErrForbidden):
		writeError(w, r, 403, "forbidden", "This staff role cannot read this resource.")
	case expectedHTTPErrorOnly(err, operator.ErrInvalid):
		writeError(w, r, 400, "invalid_request", "Use a valid explicit target and pagination values.")
	case expectedHTTPErrorOnly(err, operator.ErrNotFound):
		writeError(w, r, 404, "not_found", "The requested resource was not found.")
	default:
		writeError(w, r, 503, "operator_unavailable", "Operator evidence is unavailable. Try again later.", err)
	}
}
func operatorReporting(opts Options) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dropped := uint64(0)
		if opts.SupportReports != nil {
			dropped = opts.SupportReports.Dropped()
		}
		state := "disabled"
		if opts.Config.Reporting.Enabled {
			state = "enabled_unconfirmed"
		}
		writeJSON(w, 200, SuccessResponse{Data: map[string]any{"observed_at": time.Now().UTC(), "component": "paperboat-server", "reporting": state, "release": opts.Config.Reporting.Release, "release_configured": opts.Config.Reporting.Release != "", "issue_links_configured": opts.Config.Reporting.IssueURL != "", "dropped_records_process": dropped, "retention_days": 7, "delivery": "unverified", "source_maps": "not_observed_by_server", "other_components": "not_observed_by_server", "admin_api": opts.OperatorReporting.Status(), "signals": opts.SupportReports.SignalStatus()}})
	})
}

func registerOperatorMutations(mux *http.ServeMux, opts Options) {
	if opts.OperatorActions != nil {
		for _, route := range []struct{ pattern, kind, param string }{
			{"POST /v1/admin/accounts/{account_id}/ban", "account_ban", "account_id"},
			{"POST /v1/admin/accounts/{account_id}/unban", "account_unban", "account_id"},
			{"POST /v1/admin/teams/{team_id}/ban", "team_ban", "team_id"},
			{"POST /v1/admin/teams/{team_id}/unban", "team_unban", "team_id"},
			{"POST /v1/admin/relays/{node_id}/drain", "relay_drain", "node_id"},
			{"POST /v1/admin/relays/{node_id}/resume", "relay_resume", "node_id"},
			{"POST /v1/admin/tunnels/{tunnel_id}/disable", "tunnel_disable", "tunnel_id"},
			{"POST /v1/admin/tunnels/{tunnel_id}/enable", "tunnel_enable", "tunnel_id"},
		} {
			mux.Handle(route.pattern, operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var input operatoractions.Input
				if !decodeStrictJSON(w, r, &input) {
					return
				}
				p, _ := principalFromContext(r.Context())
				result, err := opts.OperatorActions.Execute(r.Context(), p.User.ID, r.Header.Get("Idempotency-Key"), route.kind, r.PathValue(route.param), r.Header.Get("Operator-Reason"), input)
				if operatorMutationError(w, r, err) {
					return
				}
				writeJSON(w, 200, SuccessResponse{Data: result})
			})))
		}
	}
	if opts.OperatorBilling != nil {
		mux.Handle("GET /v1/admin/billing/products", operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := principalFromContext(r.Context())
			limit := 50
			if raw := r.URL.Query().Get("limit"); raw != "" {
				var err error
				limit, err = strconv.Atoi(raw)
				if err != nil || limit < 1 || limit > 100 {
					writeError(w, r, 400, "invalid_request", "Limit must be between 1 and 100.")
					return
				}
			}
			result, err := opts.OperatorBilling.ListProducts(r.Context(), p.User.ID, r.URL.Query().Get("after"), limit)
			if operatorMutationError(w, r, err) {
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: result})
		})))
		mux.Handle("PUT /v1/admin/billing/products/{code}", operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var input billing.ProductInput
			if !decodeStrictJSON(w, r, &input) {
				return
			}
			p, _ := principalFromContext(r.Context())
			result, err := opts.OperatorBilling.PutProduct(r.Context(), p.User.ID, r.Header.Get("Idempotency-Key"), r.PathValue("code"), r.Header.Get("Operator-Reason"), input)
			if operatorMutationError(w, r, err) {
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: result})
		})))
		mux.Handle("GET /v1/admin/config/billing", operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := principalFromContext(r.Context())
			result, err := opts.OperatorBilling.Policy(r.Context(), p.User.ID)
			if operatorMutationError(w, r, err) {
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: result})
		})))
		mux.Handle("PUT /v1/admin/config/billing", operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var input billing.OperatorPolicy
			if !decodeStrictJSON(w, r, &input) {
				return
			}
			p, _ := principalFromContext(r.Context())
			result, err := opts.OperatorBilling.PutPolicy(r.Context(), p.User.ID, r.Header.Get("Idempotency-Key"), r.Header.Get("Operator-Reason"), input)
			if operatorMutationError(w, r, err) {
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: result})
		})))
	}
}
func operatorMutationError(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case expectedHTTPErrorOnly(err, operatoractions.ErrForbidden, billing.ErrOperatorBillingForbidden):
		writeError(w, r, 403, "forbidden", "This staff role cannot perform this action.")
	case expectedHTTPErrorOnly(err, operatoractions.ErrInvalid, billing.ErrOperatorBillingInvalid):
		writeError(w, r, 400, "invalid_request", "Use the documented fields and current target revision.")
	case expectedHTTPErrorOnly(err, operatoractions.ErrNotFound, billing.ErrOperatorBillingNotFound):
		writeError(w, r, 404, "not_found", "The target was not found.")
	case expectedHTTPErrorOnly(err, operatoractions.ErrConflict, billing.ErrOperatorBillingConflict):
		writeError(w, r, 409, "operator_conflict", "The target changed or the Idempotency-Key belongs to a different request. Inspect current state.")
	default:
		writeError(w, r, 503, "operator_unavailable", "The result is unavailable. Check the target and retry with the same Idempotency-Key.", err)
	}
	return true
}

func operatorStatus(opts Options) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		readiness := "unknown"
		if opts.ReadinessChecker != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			err := opts.ReadinessChecker.Ready(ctx)
			cancel()
			readiness = "ready"
			if err != nil {
				readiness = "unavailable"
			}
		}
		dimensions := map[string]any{}
		if opts.OperatorHealth != nil {
			snapshot := opts.OperatorHealth.Snapshot()
			for _, d := range telemetry.Dimensions() {
				state := snapshot.Dimensions.Get(d)
				dimensions[string(d)] = map[string]any{"status": state.Status, "code": state.Code, "since": state.Since, "retry": state.Retry, "next_retry_at": state.NextRetryAt}
			}
		}
		writeJSON(w, 200, SuccessResponse{Data: map[string]any{"observed_at": time.Now().UTC(), "readiness": readiness, "dimensions": dimensions, "providers": map[string]any{"workos_configured": opts.Config.Secrets.WorkOSClientID != "" && opts.Config.Secrets.WorkOSAPIKey != "", "stripe_configured": opts.Config.Secrets.StripeAPIKey != "", "github_configured": opts.Config.Secrets.GitHubClientID != "", "live_availability": "not_probed"}, "configuration": map[string]any{"host_offline_after_seconds": opts.Config.UserMachines.OfflineAfter.Seconds(), "request_timeout_seconds": opts.Config.HTTP.RequestTimeout.Seconds(), "billing_policy": "/v1/admin/config/billing", "deployment_settings": "managed_by_deployment"}}})
	})
}
