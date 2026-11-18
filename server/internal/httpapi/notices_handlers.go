package httpapi

import (
	"net/http"
	"strconv"

	"github.com/pinksaucepasta/paperboat/server/internal/announcements"
	"github.com/pinksaucepasta/paperboat/server/internal/customernotices"
)

// The operator's customer notices (price increase, terms change, new subprocessor, breach): preview with dry_run, then send.
// Sending is audited by the service; the operator guard has already required the target, a reason and an Idempotency-Key.
func registerOperatorNotices(mux *http.ServeMux, opts Options) {
	s := opts.CustomerNotices
	mux.Handle("GET /v1/admin/notices", operatorRoute(opts, operatorNoticeList(s)))
	mux.Handle("POST /v1/admin/notices", operatorRoute(opts, operatorNoticeSend(s)))
}

func operatorNoticeList(s *customernotices.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, after := 50, int64(0)
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > 100 {
				writeError(w, r, 400, "invalid_request", "Limit must be between 1 and 100.")
				return
			}
			limit = n
		}
		if raw := r.URL.Query().Get("after"); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n < 1 {
				writeError(w, r, 400, "invalid_request", "After must be a cursor from a previous page.")
				return
			}
			after = n
		}
		items, next, err := s.List(r.Context(), after, limit)
		if err != nil {
			writeNoticeError(w, r, err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: map[string]any{"notices": items, "next_after": next}})
	}
}

func operatorNoticeSend(s *customernotices.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in customernotices.Input
		r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		p, _ := principalFromContext(r.Context())
		out, err := s.Send(r.Context(), p.User.ID, r.Header.Get("Idempotency-Key"), in)
		if err != nil {
			writeNoticeError(w, r, err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: out})
	}
}

func writeNoticeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case expectedHTTPErrorOnly(err, customernotices.ErrInvalid, customernotices.ErrTooManyMail, announcements.ErrInvalid):
		writeError(w, r, 400, "invalid_request", announcementMessage(err, "Use the documented fields."))
	case expectedHTTPErrorOnly(err, customernotices.ErrConflict):
		writeError(w, r, 409, "operator_conflict", announcementMessage(err, "The notice conflicts with another."))
	case expectedHTTPErrorOnly(err, customernotices.ErrMailOff):
		writeError(w, r, 503, "mail_unavailable", "Transactional mail is not configured on this server, so no notice can be sent.")
	default:
		writeError(w, r, 503, "notices_unavailable", "Notices are unavailable. Try again later.", err)
	}
}
