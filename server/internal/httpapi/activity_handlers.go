package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/pinksaucepasta/paperboat-server/internal/activity"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

func activityGet(service *activity.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, 401, "unauthenticated", "Sign in to view activity.")
			return
		}
		scope, present, err := requestWorkspace(r)
		if err != nil {
			writeWorkspaceError(w, r, err)
			return
		}
		if !present {
			scope = workspace.Scope{ID: "personal"}
		}
		values := r.URL.Query()
		for key, entries := range values {
			switch key {
			case "workspace", "limit", "cursor", "machine_id", "area", "q":
			default:
				writeError(w, r, 400, "invalid_activity_query", "Choose supported activity filters.")
				return
			}
			if len(entries) != 1 {
				writeError(w, r, 400, "invalid_activity_query", "Activity filters must occur once.")
				return
			}
		}
		q := activity.Query{Cursor: values.Get("cursor"), MachineID: values.Get("machine_id"), Area: values.Get("area"), Search: values.Get("q")}
		if values.Has("limit") {
			q.Limit, err = strconv.Atoi(values.Get("limit"))
			if err != nil || q.Limit < 1 {
				writeError(w, r, 400, "invalid_activity_query", "Choose an activity limit from 1 to 200.")
				return
			}
		}
		if q.Validate() != nil {
			writeError(w, r, 400, "invalid_activity_query", "Choose valid activity filters.")
			return
		}
		out, err := service.List(r.Context(), p.User.ID, scope, q)
		switch {
		case errors.Is(err, activity.ErrInvalid):
			writeError(w, r, 400, "invalid_activity_query", "Choose valid activity filters and cursor.")
		case errors.Is(err, workspace.ErrForbidden), errors.Is(err, workspace.ErrInvalid):
			writeWorkspaceError(w, r, err)
		case err != nil:
			writeError(w, r, 500, "activity_unavailable", "Activity is unavailable. Try again.", err)
		default:
			writeJSON(w, 200, SuccessResponse{Data: out})
		}
	}
}
