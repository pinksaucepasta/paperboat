package httpapi

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

type inventoryQuery struct {
	Limit, Offset   int
	Q, State, Owner string
	Closed          *bool
}

func readInventoryQuery(w http.ResponseWriter, r *http.Request, states []string, allowClosed bool, extraKeys ...string) (inventoryQuery, bool) {
	out := inventoryQuery{Limit: 50}
	values := r.URL.Query()
	bad := func() (inventoryQuery, bool) {
		writeError(w, r, 400, "invalid_inventory_query", "Use one valid limit (1–200), nonnegative offset, and supported inventory filters.")
		return inventoryQuery{}, false
	}
	for _, key := range append([]string{"limit", "offset", "q", "state", "owner", "closed", "cursor", "sort"}, extraKeys...) {
		if len(values[key]) > 1 {
			return bad()
		}
	}
	for key := range values {
		switch key {
		case "limit", "offset", "q", "state", "owner", "closed", "cursor", "sort", "workspace":
		default:
			if !slices.Contains(extraKeys, key) {
				return bad()
			}
		}
	}
	if values.Has("cursor") {
		return bad()
	}
	if raw, ok := values["limit"]; ok {
		n, e := strconv.Atoi(raw[0])
		if e != nil || n < 1 || n > 200 {
			return bad()
		}
		out.Limit = n
	}
	if raw, ok := values["offset"]; ok {
		n, e := strconv.Atoi(raw[0])
		if e != nil || n < 0 || n > 2147483647 {
			return bad()
		}
		out.Offset = n
	}
	out.Q = values.Get("q")
	if !utf8.ValidString(out.Q) || len(out.Q) > 160 || strings.ContainsRune(out.Q, '\x00') {
		return bad()
	}
	out.State = values.Get("state")
	if out.State != "" {
		valid := false
		for _, s := range states {
			if out.State == s {
				valid = true
			}
		}
		if !valid {
			return bad()
		}
	}
	out.Owner = values.Get("owner")
	if !utf8.ValidString(out.Owner) || len(out.Owner) > 128 || strings.ContainsAny(out.Owner, " \t\r\n\x00") {
		return bad()
	}
	if raw, ok := values["closed"]; ok {
		if !allowClosed {
			return bad()
		}
		if raw[0] != "true" && raw[0] != "false" {
			return bad()
		}
		b := raw[0] == "true"
		out.Closed = &b
	}
	if sort := values.Get("sort"); sort != "" && sort != "alias" {
		return bad()
	}
	return out, true
}

func inventoryPagination(limit, offset, total, count int) map[string]any {
	var next any
	if count > 0 && offset+count < total {
		next = offset + count
	}
	return map[string]any{"limit": limit, "offset": offset, "total": total, "next_offset": next}
}

// Cursor collections use the same filter validation but reject offset semantics.
func readCursorInventoryQuery(w http.ResponseWriter, r *http.Request, states []string, extraKeys ...string) (inventoryQuery, bool) {
	values := r.URL.Query()
	if len(values["cursor"]) > 1 || values.Has("offset") {
		writeError(w, r, 400, "invalid_inventory_query", "Use one cursor and no offset.")
		return inventoryQuery{}, false
	}
	cloned := r.Clone(r.Context())
	urlCopy := *r.URL
	cloned.URL = &urlCopy
	values.Del("cursor")
	cloned.URL.RawQuery = values.Encode()
	return readInventoryQuery(w, cloned, states, false, extraKeys...)
}

// readMachineIDFilter reads the optional machine_id filter of a list (one machine's terminals, previews or
// tunnels): an exact machine id, at most 128 bytes, no whitespace or NUL.
func readMachineIDFilter(w http.ResponseWriter, r *http.Request) (string, bool) {
	machineID := r.URL.Query().Get("machine_id")
	if !utf8.ValidString(machineID) || len(machineID) > 128 || strings.ContainsAny(machineID, " \t\r\n\x00") {
		writeError(w, r, 400, "invalid_inventory_query", "Use one valid limit (1–200), nonnegative offset, and supported inventory filters.")
		return "", false
	}
	return machineID, true
}
