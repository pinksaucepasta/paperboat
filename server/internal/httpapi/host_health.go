package httpapi

import (
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/hosthealth"
	"github.com/pinksaucepasta/paperboat/server/internal/nodeauth"
)

// The production canary runner pushes host capacity and canary state with a dedicated credential; the admin panel reads the
// latest of each, so a failing host or canary shows in the panel instead of only in the runner's own log.
func registerHostHealthRoutes(mux *http.ServeMux, opts Options) {
	s := opts.HostHealth
	if s == nil {
		return
	}
	mux.HandleFunc("POST /v1/operations/host-health/reports", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !s.Authenticate(nodeauth.Bearer(r)) {
			writeError(w, r, 401, "host_health_unauthorized", "A dedicated host health reporting credential is required.")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 48<<10)
		var report hosthealth.Report
		if !decodeStrictJSON(w, r, &report) {
			return
		}
		switch err := s.Ingest(r.Context(), report); err {
		case nil:
			writeJSON(w, 200, SuccessResponse{Data: map[string]string{"status": "recorded"}})
		case hosthealth.ErrInvalid:
			writeError(w, r, 400, "invalid_request", "Report a known source with a JSON object payload of at most 32 KiB.")
		default:
			writeError(w, r, 503, "host_health_unavailable", "The report could not be recorded. Retry.", err)
		}
	})
	if opts.OperatorAuth == nil {
		return
	}
	mux.Handle("GET /v1/admin/host-health", operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entries, err := s.Snapshot(r.Context())
		if err != nil {
			writeError(w, r, 503, "host_health_unavailable", "Host health could not be read. Retry.", err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: map[string]any{"reports": entries}})
	})))
}
