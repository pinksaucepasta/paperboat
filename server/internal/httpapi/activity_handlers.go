package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/activity"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// activityRequest parses the audit log filters shared by list and export. It writes the error
// response and returns false when the request is invalid.
func activityRequest(w http.ResponseWriter, r *http.Request, extra ...string) (string, workspace.Scope, activity.Query, bool) {
	p, ok := principalFromContext(r.Context())
	if !ok {
		writeError(w, r, 401, "unauthenticated", "Sign in to view activity.")
		return "", workspace.Scope{}, activity.Query{}, false
	}
	scope, present, err := requestWorkspace(r)
	if err != nil {
		writeWorkspaceError(w, r, err)
		return "", scope, activity.Query{}, false
	}
	if !present {
		scope = workspace.Scope{ID: "personal"}
	}
	values := r.URL.Query()
	allowed := map[string]bool{"workspace": true, "limit": true, "cursor": true, "machine_id": true, "area": true, "q": true,
		"actor": true, "action": true, "target": true, "result": true, "since": true, "until": true}
	for _, key := range extra {
		allowed[key] = true
	}
	for key, entries := range values {
		if !allowed[key] {
			writeError(w, r, 400, "invalid_activity_query", "Choose supported activity filters.")
			return "", scope, activity.Query{}, false
		}
		if len(entries) != 1 {
			writeError(w, r, 400, "invalid_activity_query", "Activity filters must occur once.")
			return "", scope, activity.Query{}, false
		}
	}
	q := activity.Query{Cursor: values.Get("cursor"), MachineID: values.Get("machine_id"), Area: values.Get("area"), Search: values.Get("q"),
		Actor: values.Get("actor"), Action: values.Get("action"), Target: values.Get("target"), Result: values.Get("result")}
	if values.Has("limit") {
		q.Limit, err = strconv.Atoi(values.Get("limit"))
		if err != nil || q.Limit < 1 {
			writeError(w, r, 400, "invalid_activity_query", "Choose an activity limit from 1 to 200.")
			return "", scope, q, false
		}
	}
	if q.Since, err = activityTime(values, "since"); err == nil {
		q.Until, err = activityTime(values, "until")
	}
	if err != nil || q.Validate() != nil {
		writeError(w, r, 400, "invalid_activity_query", "Choose valid activity filters (times are RFC 3339).")
		return "", scope, q, false
	}
	return p.User.ID, scope, q, true
}

func activityTime(values url.Values, key string) (time.Time, error) {
	if !values.Has(key) {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, values.Get(key))
}

func activityGet(service *activity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		account, scope, q, ok := activityRequest(w, r)
		if !ok {
			return
		}
		out, err := service.List(r.Context(), account, scope, q)
		writeActivityResult(w, r, err, func() { writeJSON(w, 200, SuccessResponse{Data: out}) })
	}
}

// activityExport streams the audit log as a CSV or JSON file (team workspaces).
func activityExport(service *activity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		account, scope, q, ok := activityRequest(w, r, "format")
		if !ok {
			return
		}
		format := r.URL.Query().Get("format")
		if format != "csv" && format != "json" {
			writeError(w, r, 400, "invalid_activity_query", "Choose an export format: csv or json.")
			return
		}
		out := &deferredHeader{w: w, format: format, team: scope.ID}
		err := service.Export(r.Context(), account, scope, q, format, out)
		if out.started {
			// The file is already streaming; a late error can only end it early.
			return
		}
		writeActivityResult(w, r, err, func() { out.start() })
	}
}

// deferredHeader sends the download headers with the first byte, so errors found before any
// event was written still get a normal JSON error response.
type deferredHeader struct {
	w            http.ResponseWriter
	format, team string
	started      bool
}

func (d *deferredHeader) start() {
	if d.started {
		return
	}
	d.started = true
	contentType := "text/csv; charset=utf-8"
	if d.format == "json" {
		contentType = "application/json"
	}
	d.w.Header().Set("Content-Type", contentType)
	d.w.Header().Set("Content-Disposition", `attachment; filename="paperboat-audit-`+d.team+`-`+time.Now().UTC().Format("20060102T150405Z")+`.`+d.format+`"`)
	d.w.WriteHeader(http.StatusOK)
}

func (d *deferredHeader) Write(p []byte) (int, error) {
	d.start()
	return d.w.Write(p)
}

func writeActivityResult(w http.ResponseWriter, r *http.Request, err error, ok func()) {
	switch {
	case errors.Is(err, activity.ErrInvalid):
		writeError(w, r, 400, "invalid_activity_query", "Choose valid activity filters and cursor.")
	case errors.Is(err, activity.ErrExportNeedsTeam):
		writeError(w, r, 400, "activity_export_team_only", "Audit log export is available in team workspaces.")
	case errors.Is(err, workspace.ErrForbidden), errors.Is(err, workspace.ErrInvalid):
		writeWorkspaceError(w, r, err)
	case err != nil:
		writeError(w, r, 500, "activity_unavailable", "Activity is unavailable. Try again.", err)
	default:
		ok()
	}
}
