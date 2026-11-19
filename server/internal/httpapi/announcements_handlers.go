package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/announcements"
)

// The operator's announcements: list, write, edit and retract. Every write is audited by the service; the operator guard
// has already required the target, a reason and an Idempotency-Key.
func registerOperatorAnnouncements(mux *http.ServeMux, opts Options) {
	s := opts.Announcements
	mux.Handle("GET /v1/admin/announcements", operatorRoute(opts, operatorAnnouncementList(s)))
	mux.Handle("GET /v1/admin/node-notices", operatorRoute(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		notices, err := s.ForOperators(r.Context())
		if err != nil {
			writeError(w, r, 503, "announcements_unavailable", "Notices could not be read. Retry.", err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: map[string]any{"notices": notices}})
	})))
	mux.Handle("POST /v1/admin/announcements", operatorRoute(opts, operatorAnnouncementWrite(s, "")))
	mux.Handle("PUT /v1/admin/announcements/{announcement_id}", operatorRoute(opts, operatorAnnouncementWrite(s, "announcement_id")))
	mux.Handle("POST /v1/admin/announcements/{announcement_id}/retract", operatorRoute(opts, operatorAnnouncementRetract(s)))
}

func operatorAnnouncementList(s *announcements.Service) http.HandlerFunc {
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
		page, err := s.List(r.Context(), r.URL.Query().Get("state"), after, limit)
		if err != nil {
			writeAnnouncementError(w, r, err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: page})
	}
}

func operatorAnnouncementWrite(s *announcements.Service, param string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in announcements.Input
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		p, _ := principalFromContext(r.Context())
		var out announcements.Announcement
		var err error
		if param == "" {
			out, err = s.Create(r.Context(), p.User.ID, r.Header.Get("Idempotency-Key"), in)
		} else {
			out, err = s.Update(r.Context(), p.User.ID, r.Header.Get("Idempotency-Key"), r.PathValue(param), in)
		}
		if err != nil {
			writeAnnouncementError(w, r, err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: out})
	}
}

func operatorAnnouncementRetract(s *announcements.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, _ := principalFromContext(r.Context())
		out, err := s.Retract(r.Context(), p.User.ID, r.Header.Get("Idempotency-Key"), r.PathValue("announcement_id"))
		if err != nil {
			writeAnnouncementError(w, r, err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: out})
	}
}

// announcementMessage is the sentence after the sentinel: "invalid announcement: the title is 1 to 80 characters".
func announcementMessage(err error, fallback string) string {
	if _, rest, ok := strings.Cut(err.Error(), ": "); ok {
		return strings.ToUpper(rest[:1]) + rest[1:] + "."
	}
	return fallback
}

func writeAnnouncementError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case expectedHTTPErrorOnly(err, announcements.ErrInvalid):
		writeError(w, r, 400, "invalid_request", announcementMessage(err, "Use the documented fields."))
	case expectedHTTPErrorOnly(err, announcements.ErrNotFound):
		writeError(w, r, 404, "not_found", "The announcement was not found.")
	case expectedHTTPErrorOnly(err, announcements.ErrConflict):
		writeError(w, r, 409, "operator_conflict", announcementMessage(err, "The announcement conflicts with another."))
	default:
		writeError(w, r, 503, "announcements_unavailable", "Announcements are unavailable. Try again later.", err)
	}
}

// The user's side: what the console shows (bell and ribbon), and what they did with it.
func announcementsGet(s *announcements.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		view, err := s.ForUser(r.Context(), p.User.ID)
		if err != nil {
			writeError(w, r, 503, "announcements_unavailable", "Announcements could not be read. Retry.", err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: view})
	}
}

func announcementsRead(s *announcements.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		var body struct {
			IDs []string `json:"ids"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		if !decodeStrictJSON(w, r, &body) {
			return
		}
		if err := s.MarkRead(r.Context(), p.User.ID, body.IDs); err != nil {
			writeAnnouncementUserError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func announcementsDismiss(s *announcements.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		if err := s.Dismiss(r.Context(), p.User.ID, r.PathValue("announcement_id")); err != nil {
			writeAnnouncementUserError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func writeAnnouncementUserError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case expectedHTTPErrorOnly(err, announcements.ErrInvalid):
		writeError(w, r, 400, "invalid_request", "Mark at most 50 notices at a time.")
	case expectedHTTPErrorOnly(err, announcements.ErrNotFound):
		writeError(w, r, 404, "not_found", "That banner is gone or cannot be dismissed.")
	default:
		writeError(w, r, 503, "announcements_unavailable", "Announcements could not be saved. Retry.", err)
	}
}
