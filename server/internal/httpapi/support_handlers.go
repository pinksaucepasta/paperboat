package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/felixge/httpsnoop"
	"github.com/getsentry/sentry-go"
	serverdiagnostics "github.com/pinksaucepasta/paperboat/server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

type supportContextKey struct{}
type supportContext struct {
	reference string
	service   *supportreport.Service
	logger    *slog.Logger
	owner     *atomic.Pointer[string]
}

func supportReference(ctx context.Context) string {
	v, _ := ctx.Value(supportContextKey{}).(supportContext)
	return v.reference
}
func supportMiddleware(service *supportreport.Service, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reference := r.Header.Get("Support-Reference")
		if !supportreport.ValidReference(reference) {
			reference = supportreport.NewReference()
		}
		w.Header().Set("Support-Reference", reference)
		if serverdiagnostics.NormalizeRequestID(r.Header.Get("Correlation-Id")) == "" {
			r = r.WithContext(serverdiagnostics.WithCorrelationID(r.Context(), reference))
			w.Header().Set("Correlation-Id", reference)
		}
		ctx := context.WithValue(r.Context(), supportContextKey{}, supportContext{reference: reference, service: service, logger: logger, owner: &atomic.Pointer[string]{}})
		ctx, finish := service.Begin(ctx, supportOperation(r.URL.Path), reference, r.Header.Get("sentry-trace"))
		outcome := "failed"
		defer func() { finish(outcome) }()
		measured := httpsnoop.CaptureMetrics(next, w, r.WithContext(ctx))
		outcome = "success"
		if measured.Code >= 500 {
			outcome = "failed"
		} else if measured.Code >= 400 {
			outcome = "rejected"
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			outcome = "canceled"
		}
	})
}
func captureSupportFailure(r *http.Request, status int, code string, stack *sentry.Stacktrace, causes ...error) {
	if status < 500 {
		return
	}
	sc, _ := r.Context().Value(supportContextKey{}).(supportContext)
	owner := ""
	if sc.owner != nil {
		if id := sc.owner.Load(); id != nil {
			owner = *id
		}
	}
	if p, ok := principalFromContext(r.Context()); ok {
		owner = p.User.ID
	}
	stage := "http_handler"
	if code == "panic" {
		stage = "panic_recovery"
	}
	var cause error
	if len(causes) > 0 {
		cause = causes[0]
	}
	ctx := supportreport.WithFailureIdentity(r.Context(), owner, requestIDFromContext(r.Context()), sc.reference)
	cause = supportreport.WithDiagnosticStatus(status, cause)
	fault := supportreport.ProjectFaultWithStack(ctx, "paperboat-server", supportOperation(r.URL.Path), stage, code, cause, stack)
	if sc.service != nil {
		fault = sc.service.CaptureFailureWithStack(ctx, "paperboat-server", supportOperation(r.URL.Path), stage, code, cause, stack)
	}
	if sc.logger != nil {
		level := slog.LevelError
		if fault.Severity == "warning" {
			level = slog.LevelWarn
		} else if fault.Severity == "info" {
			level = slog.LevelInfo
		}
		serverdiagnostics.LoggerWithRequest(r.Context(), sc.logger).Log(r.Context(), level, "HTTP operation failed", fault.LogAttrs()...)
	}
}

func logHTTPRejection(r *http.Request, status int, code string, err error) {
	sc, _ := r.Context().Value(supportContextKey{}).(supportContext)
	logger := sc.logger
	if logger == nil {
		logger = slog.Default()
	}
	ctx := supportreport.WithFailureIdentity(r.Context(), "", requestIDFromContext(r.Context()), sc.reference)
	fault := supportreport.ProjectFault(ctx, "paperboat-server", supportOperation(r.URL.Path), "http_handler", code, supportreport.WithDiagnosticStatus(status, err))
	fault.Outcome, fault.Severity = "rejected", "warning"
	serverdiagnostics.LoggerWithRequest(r.Context(), logger).WarnContext(r.Context(), "HTTP request rejected", fault.LogAttrs()...)
}

// supportUploadLookup tells whether the caller uploaded a diagnostic bundle under a support reference.
type supportUploadLookup interface {
	Uploaded(ctx context.Context, userID, reference string) (bool, error)
}

func supportLookup(service *supportreport.Service, uploads supportUploadLookup, operator bool, issueURL string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, 401, "unauthenticated", "Authentication is required.")
			return
		}
		owner := p.User.ID
		if operator {
			owner = r.URL.Query().Get("owner_id")
		}
		ref := r.PathValue("reference")
		if !supportreport.ValidReference(ref) {
			writeError(w, r, 400, "invalid_support_reference", "Use the complete PB support reference.")
			return
		}
		records, err := service.Lookup(r.Context(), p.User.ID, owner, ref, operator)
		if errors.Is(err, supportreport.ErrInvalid) {
			writeError(w, r, 400, "invalid_support_lookup", "Use a valid support reference and account identifier.")
			return
		}
		if err != nil {
			writeError(w, r, 503, "support_unavailable", "Support evidence is unavailable. Keep the reference and try again later.", err)
			return
		}
		if !operator {
			// End users receive status only; no internal code locations, ownership or
			// third-party identifiers can escape via the account endpoint.
			state := "unavailable"
			reporting := "unavailable"
			if len(records) > 0 {
				state = "recorded"
				reporting = records[0].Reporting
			} else if uploads != nil {
				// A bundle uploaded by `pb bugreport --upload` is recorded although it is not a fault.
				uploaded, uploadErr := uploads.Uploaded(r.Context(), p.User.ID, ref)
				if uploadErr != nil {
					writeError(w, r, 503, "support_unavailable", "Support evidence is unavailable. Keep the reference and try again later.", uploadErr)
					return
				}
				if uploaded {
					state, reporting = "recorded", "diagnostic_bundle"
				}
			}
			writeJSON(w, 200, SuccessResponse{Data: map[string]any{"support_reference": ref, "status": state, "reporting": reporting, "message": "Keep this reference when contacting support. Unavailable evidence may be local, expired, or not recorded."}})
			return
		}
		truncated := len(records) > 128
		if truncated {
			records = records[:128]
		}
		groups := map[string][]supportreport.Record{}
		for _, record := range records {
			groups[record.OwnerID] = append(groups[record.OwnerID], record)
		}
		links := map[string]string{}
		if issueURL != "" {
			for _, record := range records {
				if record.EventID != "" {
					links[record.EventID] = issueURL + "?query=" + url.QueryEscape(record.EventID)
				}
			}
		}
		searchURL := ""
		if issueURL != "" {
			searchURL = issueURL + "?query=" + url.QueryEscape("support_reference:"+ref)
		}
		state := "available"
		if len(records) == 0 {
			state = "unavailable"
		}
		writeJSON(w, 200, SuccessResponse{Data: map[string]any{"support_reference": ref, "owner_id": owner, "status": state, "records_by_owner": groups, "truncated": truncated, "sentry_links": links, "sentry_search_url": searchURL, "dropped_records": service.Dropped(), "message": "Sentry event IDs indicate an attempted capture, not confirmed receipt. Missing evidence may be expired, filtered, local, or dropped."}})
	})
}

func bindSupportOwner(ctx context.Context, owner string) {
	sc, _ := ctx.Value(supportContextKey{}).(supportContext)
	if sc.owner != nil {
		sc.owner.Store(&owner)
	}
}

// Classify only fixed product families. Raw paths, including machine codes and
// resource identifiers, never become logs, span names or metric dimensions.
func supportOperation(path string) string {
	if path == "/healthz" || path == "/readyz" || path == "/metrics" {
		return "http_health"
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 2 || parts[0] != "v1" {
		return "http_other"
	}
	switch parts[1] {
	case "auth", "login", "oauth":
		return "http_auth"
	case "accounts", "account", "users", "me":
		return "http_accounts"
	case "teams", "team":
		return "http_teams"
	case "billing", "stripe", "webhooks", "subscriptions", "workspaces":
		return "http_billing"
	case "admin", "operations":
		return "http_admin"
	case "machines", "user-machines", "helper-enrollments", "helper-identity-renewals", "runtime-observations", "machine-control-credentials", "machine-control-renewals", "machine-enrollments", "machine-installation-failures", "endpoints", "helper-trust":
		return "http_machines"
	case "terminal", "terminal-sessions", "connect", "sessions", "ssh":
		return "http_terminal"
	case "environments", "env", "environment-variables", "environment-scopes", "environment-vaults", "environment-vault":
		return "http_environment"
	case "config", "configuration", "sync", "machine-services", "runtime-machine-services", "config-repositories", "config-sync", "client-configuration", "helper-runtime-policies":
		return "http_config"
	case "files", "transfers", "transfer-destination-default":
		return "http_files"
	case "tunnels", "tunnel", "browser-access", "routes":
		return "http_tunnels"
	case "previews", "preview":
		return "http_previews"
	case "releases", "helper-releases":
		return "http_releases"
	case "edge", "nodes", "control":
		return "http_edge"
	case "network", "peers", "p2p", "selfhost", "native-private-access", "peer-network", "machine-peer-identity", "machine-peer-network", "e2ee":
		return "http_network"
	case "support", "diagnostic-upload-intents", "inspector":
		return "http_support"
	default:
		return "http_other"
	}
}
