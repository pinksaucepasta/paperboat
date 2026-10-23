package httpapi

import (
	"net/http"
	"reflect"
	"strconv"

	"github.com/pinksaucepasta/paperboat/server/internal/backupreport"
	"github.com/pinksaucepasta/paperboat/server/internal/nodeauth"
	"github.com/pinksaucepasta/paperboat/server/internal/operatorreporting"
)

func registerBackupReportRoute(mux *http.ServeMux, opts Options) {
	if opts.OperatorBackups == nil {
		return
	}
	mux.HandleFunc("POST /v1/operations/backups/reports", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		token := nodeauth.Bearer(r)
		if !opts.OperatorBackups.Authenticate(token) {
			writeError(w, r, 401, "backup_report_unauthorized", "A dedicated backup reporting credential is required.")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		var report backupreport.Report
		if !decodeStrictJSON(w, r, &report) {
			return
		}
		if err := opts.OperatorBackups.Ingest(r.Context(), token, report); err != nil {
			operatorDiagnosticsError(w, r, err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: map[string]string{"status": "recorded", "run_id": report.RunID}})
	})
}
func operatorPageLimit(w http.ResponseWriter, r *http.Request, max int) (int, bool) {
	limit := min(50, max)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 1 || n > max {
			writeError(w, r, 400, "invalid_request", "Use a bounded positive limit.")
			return 0, false
		}
		limit = n
	}
	return limit, true
}
func registerOperatorDiagnostics(mux *http.ServeMux, opts Options) {
	if service := opts.OperatorBackups; service != nil {
		mux.Handle("GET /v1/admin/backups", operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := principalFromContext(r.Context())
			out, err := service.Status(r.Context(), p.User.ID)
			if err != nil {
				operatorDiagnosticsError(w, r, err)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: out})
		})))
		mux.Handle("GET /v1/admin/backups/runs", operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			limit, ok := operatorPageLimit(w, r, 100)
			if !ok {
				return
			}
			p, _ := principalFromContext(r.Context())
			out, err := service.List(r.Context(), p.User.ID, r.URL.Query().Get("after"), limit)
			if err != nil {
				operatorDiagnosticsError(w, r, err)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: out})
		})))
	}
	if service := opts.OperatorReporting; service != nil {
		for _, route := range []struct {
			pattern, kind string
			max           int
		}{
			{"GET /v1/admin/support", "local", 100},
			{"GET /v1/admin/sentry/issues", "issues", 100},
			{"GET /v1/admin/sentry/issues/{issue_id}", "issue", 10},
			{"GET /v1/admin/sentry/references/{reference}", "reference", 1},
		} {
			mux.Handle(route.pattern, operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				limit, ok := operatorPageLimit(w, r, route.max)
				if !ok {
					return
				}
				p, _ := principalFromContext(r.Context())
				var out any
				var err error
				switch route.kind {
				case "local":
					out, err = service.ListLocalReferences(r.Context(), p.User, r.URL.Query().Get("after"), limit)
				case "issues":
					out, err = service.ListIssues(r.Context(), p.User, r.URL.Query().Get("cursor"), limit)
				case "issue":
					out, err = service.GetIssue(r.Context(), p.User, r.PathValue("issue_id"), limit)
				case "reference":
					if !r.URL.Query().Has("owner_id") {
						writeError(w, r, 400, "invalid_request", "Select owner_id from the local reference feed; use an explicit empty value for unattributed evidence.")
						return
					}
					out, err = service.ResolveReference(r.Context(), p.User, r.PathValue("reference"), r.URL.Query().Get("owner_id"))
				}
				if err != nil {
					operatorDiagnosticsError(w, r, err)
					return
				}
				writeJSON(w, 200, SuccessResponse{Data: out})
			})))
		}
		mux.Handle("GET /v1/admin/sentry/logs", operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			limit, ok := operatorPageLimit(w, r, 100)
			if !ok {
				return
			}
			period := r.URL.Query().Get("period")
			if period == "" {
				period = "24h"
			}
			p, _ := principalFromContext(r.Context())
			out, err := service.ListLogs(r.Context(), p.User, period, r.URL.Query().Get("cursor"), r.URL.Query().Get("reference"), r.URL.Query().Get("component"), limit)
			if err != nil {
				operatorDiagnosticsError(w, r, err)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: out})
		})))
		mux.Handle("GET /v1/admin/sentry/traces/{trace_id}", operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			period := r.URL.Query().Get("period")
			if period == "" {
				period = "24h"
			}
			p, _ := principalFromContext(r.Context())
			out, err := service.GetTrace(r.Context(), p.User, r.PathValue("trace_id"), period)
			if err != nil {
				operatorDiagnosticsError(w, r, err)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: out})
		})))
		mux.Handle("GET /v1/admin/sentry/references/{reference}/signals", operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			limit, ok := operatorPageLimit(w, r, 100)
			if !ok {
				return
			}
			period := r.URL.Query().Get("period")
			if period == "" {
				period = "24h"
			}
			p, _ := principalFromContext(r.Context())
			out, err := service.CorrelateReference(r.Context(), p.User, r.PathValue("reference"), period, limit)
			if err != nil {
				operatorDiagnosticsError(w, r, err)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: out})
		})))
		mux.Handle("GET /v1/admin/sentry/metrics/catalog", operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			period := r.URL.Query().Get("period")
			if period == "" {
				period = "24h"
			}
			p, _ := principalFromContext(r.Context())
			out, err := service.MetricCatalog(r.Context(), p.User, period)
			if err != nil {
				operatorDiagnosticsError(w, r, err)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: out})
		})))
		mux.Handle("GET /v1/admin/sentry/metrics", operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			period := r.URL.Query().Get("period")
			if period == "" {
				period = "24h"
			}
			p, _ := principalFromContext(r.Context())
			out, err := service.Metrics(r.Context(), p.User, r.URL.Query().Get("metric"), r.URL.Query().Get("component"), period, operatorreporting.FleetFilter{Region: r.URL.Query().Get("region"), Instance: r.URL.Query().Get("instance"), Worker: r.URL.Query().Get("worker")})
			if err != nil {
				operatorDiagnosticsError(w, r, err)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: out})
		})))
		mux.Handle("GET /v1/admin/sentry/outcomes", operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			period := r.URL.Query().Get("period")
			if period == "" {
				period = "24h"
			}
			p, _ := principalFromContext(r.Context())
			out, err := service.IngestionOutcomes(r.Context(), p.User, r.URL.Query().Get("project"), period)
			if err != nil {
				operatorDiagnosticsError(w, r, err)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: out})
		})))
		mux.Handle("GET /v1/admin/sentry/monitors", operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			limit, ok := operatorPageLimit(w, r, 100)
			if !ok {
				return
			}
			p, _ := principalFromContext(r.Context())
			out, err := service.ListMonitors(r.Context(), p.User, r.URL.Query().Get("project"), r.URL.Query().Get("cursor"), limit)
			if err != nil {
				operatorDiagnosticsError(w, r, err)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: out})
		})))
		mux.Handle("GET /v1/admin/sentry/monitors/{slug}/checkins", operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			limit, ok := operatorPageLimit(w, r, 100)
			if !ok {
				return
			}
			p, _ := principalFromContext(r.Context())
			out, err := service.ListMonitorCheckIns(r.Context(), p.User, r.URL.Query().Get("project"), r.PathValue("slug"), r.URL.Query().Get("cursor"), limit)
			if err != nil {
				operatorDiagnosticsError(w, r, err)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: out})
		})))
		mux.Handle("GET /v1/admin/sentry/detectors", operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			limit, ok := operatorPageLimit(w, r, 100)
			if !ok {
				return
			}
			p, _ := principalFromContext(r.Context())
			out, err := service.ListDetectors(r.Context(), p.User, r.URL.Query().Get("project"), r.URL.Query().Get("cursor"), limit)
			if err != nil {
				operatorDiagnosticsError(w, r, err)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: out})
		})))
	}
}
func operatorDiagnosticsError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case expectedHTTPErrorOnly(err, backupreport.ErrInvalid, operatorreporting.ErrInvalid):
		writeError(w, r, 400, "invalid_request", "Use valid evidence, target and pagination fields.")
	case expectedHTTPErrorOnly(err, backupreport.ErrForbidden, operatorreporting.ErrForbidden):
		writeError(w, r, 403, "forbidden", "Current authority does not permit this action.")
	case expectedHTTPErrorOnly(err, backupreport.ErrConflict):
		writeError(w, r, 409, "backup_report_conflict", "The run conflicts with recorded evidence or its backup has not been recorded yet. Retry the unchanged backup report before its restore report.")
	case expectedHTTPErrorOnly(err, operatorreporting.ErrDisabled):
		writeError(w, r, 503, "sentry_not_configured", "The Sentry admin API has not been configured.", err)
	case expectedHTTPErrorOnly(err, operatorreporting.ErrNotFound):
		writeError(w, r, 404, "not_found", "No matching issue evidence is available.")
	case expectedHTTPErrorOnly(err, operatorreporting.ErrRateLimited):
		w.Header().Set("Retry-After", "60")
		writeError(w, r, 429, "sentry_rate_limited", "Sentry is rate limiting requests. Retry later.")
	case expectedHTTPErrorOnly(err, operatorreporting.ErrUnsupported):
		writeError(w, r, 501, "sentry_capability_unsupported", "The configured Sentry provider does not support this capability.", err)
	default:
		writeError(w, r, 503, "operator_evidence_unavailable", "Evidence could not be retrieved or recorded. Retry later; reporting retries must retain the exact run ID and content.", err)
	}
}

// expectedHTTPErrorOnly maps an expected public response only when the
// complete, bounded unwrap tree contains no independent operational cause.
// Unknown, cyclic, typed-nil, or truncated errors stay on the 503 path.
func expectedHTTPErrorOnly(err error, expected ...error) (matched bool) {
	defer func() {
		if recover() != nil {
			matched = false
		}
	}()
	ancestors := make(map[error]struct{}, 8)
	visited := 0
	var visit func(error) bool
	visit = func(current error) bool {
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		if value.Comparable() {
			if _, exists := ancestors[current]; exists {
				return false
			}
			ancestors[current] = struct{}{}
			defer delete(ancestors, current)
		}
		visited++
		if visited > 8 {
			return false
		}
		for _, sentinel := range expected {
			if sameHTTPError(current, sentinel) {
				return true
			}
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return false
			}
			for _, child := range children {
				if !visit(child) {
					return false
				}
			}
			return true
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			return visit(wrapped.Unwrap())
		}
		return false
	}
	return visit(err)
}

func sameHTTPError(current, sentinel error) bool {
	if current == nil || sentinel == nil {
		return current == nil && sentinel == nil
	}
	left, right := reflect.ValueOf(current), reflect.ValueOf(sentinel)
	return left.Type() == right.Type() && left.Comparable() && left.Interface() == right.Interface()
}
