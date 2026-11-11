package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/abusereports"
	"github.com/pinksaucepasta/paperboat/server/internal/ratelimit"
)

// abuseReportSubmit takes an anonymous abuse report (edge abuse plan 3.6.2), bounded per client
// network. The answer carries only the report id; nothing about the reported link is disclosed.
func abuseReportSubmit(service *abusereports.Service, limiter *ratelimit.Limiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := limiter.Allow("abuse-report:" + resolvedRequestNetwork(r)); !ok {
			writeRateLimited(w, r, wait)
			return
		}
		var input abusereports.Input
		if !decodeStrictJSON(w, r, &input) {
			return
		}
		report, err := service.Submit(r.Context(), input)
		if errors.Is(err, abusereports.ErrInvalid) {
			writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "abuse_report_unavailable", "The report could not be recorded. Try again, or email it instead.", err)
			return
		}
		writeJSON(w, http.StatusAccepted, SuccessResponse{Data: map[string]string{"id": report.ID}})
	}
}

// abuseReportList is the operators' queue: newest first, optionally one hostname, paged by created_at.
func abuseReportList(service *abusereports.Service) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := 50
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > 100 {
				writeError(w, r, http.StatusBadRequest, "invalid_request", "Limit must be between 1 and 100.")
				return
			}
			limit = n
		}
		var before time.Time
		if raw := r.URL.Query().Get("before"); raw != "" {
			parsed, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				writeError(w, r, http.StatusBadRequest, "invalid_request", "before must be an RFC 3339 time.")
				return
			}
			before = parsed
		}
		reports, err := service.List(r.Context(), r.URL.Query().Get("hostname"), before, limit)
		if err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "abuse_reports_unavailable", "Abuse reports are unavailable. Try again later.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: reports})
	})
}
