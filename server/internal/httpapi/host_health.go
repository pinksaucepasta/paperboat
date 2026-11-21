package httpapi

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/canonicaljson"
	"github.com/pinksaucepasta/paperboat/server/internal/hosthealth"
)

// The production canary runner and the gated deploy push host capacity, canary state and the last deploy outcome as an
// operations operator (no separate credential); the admin panel reads the latest of each, plus each edge's certificate
// renewal from its own heartbeat, so a failure shows in the panel instead of only in a runner's log.
func registerHostHealthRoutes(mux *http.ServeMux, opts Options) {
	s := opts.HostHealth
	if s == nil || opts.OperatorAuth == nil {
		return
	}
	mux.Handle("POST /v1/admin/host-health/reports", operatorRoute(opts, hostHealthReport(s)))
	mux.Handle("GET /v1/admin/host-health", operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entries, err := s.Snapshot(r.Context())
		if err != nil {
			writeError(w, r, 503, "host_health_unavailable", "Host health could not be read. Retry.", err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: map[string]any{"reports": entries}})
	})))
}

// hostHealthReport records one operator report; operatorRoute has already authenticated, confirmed and audited it.
func hostHealthReport(s *hosthealth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Exact field names only, no duplicates: the source the operator guard confirmed and audited (the "source" key)
		// is the row written; a case-folded "Source" or a repeated key is refused.
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 48<<10))
		var fields map[string]json.RawMessage
		var report hosthealth.Report
		if err != nil || canonicaljson.RejectDuplicateFields(raw) != nil || json.Unmarshal(raw, &fields) != nil || len(fields) != 2 ||
			fields["source"] == nil || fields["payload"] == nil || json.Unmarshal(fields["source"], &report.Source) != nil || report.Source != r.Header.Get("Operator-Target") {
			writeError(w, r, 400, "validation_failed", "Request body must match the documented schema.")
			return
		}
		report.Payload = fields["payload"]
		switch err := s.Ingest(r.Context(), report); err {
		case nil:
			writeJSON(w, 200, SuccessResponse{Data: map[string]string{"status": "recorded"}})
		case hosthealth.ErrInvalid:
			writeError(w, r, 400, "invalid_request", "Report hosts, canary or deploy with a JSON object payload of at most 32 KiB.")
		default:
			writeError(w, r, 503, "host_health_unavailable", "The report could not be recorded. Retry.", err)
		}
	}
}
