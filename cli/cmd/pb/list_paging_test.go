package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

const pagingTestTotal = 120

func pagingTestMachines(from, limit int) []map[string]any {
	items := []map[string]any{}
	for i := from; i < from+limit && i < pagingTestTotal; i++ {
		items = append(items, map[string]any{"id": fmt.Sprintf("machine_%03d", i), "alias": fmt.Sprintf("m%03d", i), "state": "online", "tags": []string{"tag:prod"}})
	}
	return items
}

// PQ-04: pb machine list reads one bounded page by the server's keyset cursor and shows the server's total; the
// tag filter runs on the server.
func TestMachineListShowsOnePageAndTheServersTotal(t *testing.T) {
	requested, tag := 0, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/machines" || r.URL.Query().Has("offset") {
			http.NotFound(w, r)
			return
		}
		requested++
		tag = r.URL.Query().Get("tag")
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		from := 0
		if cursor := r.URL.Query().Get("cursor"); cursor != "" {
			from, _ = strconv.Atoi(strings.TrimPrefix(cursor, "c"))
		}
		items := pagingTestMachines(from, limit)
		data := map[string]any{"items": items, "total": pagingTestTotal}
		if from+len(items) < pagingTestTotal {
			data["next_cursor"] = fmt.Sprintf("c%d", from+len(items))
		}
		writeAPIData(t, w, data)
	}))
	defer srv.Close()
	code, out, errOut := runPolicyCLI(t, srv.URL, "machine", "list")
	if code != 0 || strings.Count(out, "machine_") != defaultListLimit || requested != 1 || !strings.Contains(errOut, "Showing 50 of 120 machines") {
		t.Fatalf("code=%d rows=%d requests=%d err=%q", code, strings.Count(out, "machine_"), requested, errOut)
	}
	requested = 0
	code, out, _ = runPolicyCLI(t, srv.URL, "machine", "list", "--all")
	if code != 0 || strings.Count(out, "machine_") != pagingTestTotal || requested != 1 {
		t.Fatalf("--all: code=%d rows=%d requests=%d", code, strings.Count(out, "machine_"), requested)
	}
	requested = 0
	code, out, _ = runPolicyCLI(t, srv.URL, "machine", "list", "--tag", "prod", "--limit", "10")
	if code != 0 || strings.Count(out, "machine_") != 10 || requested != 1 || tag != "prod" {
		t.Fatalf("--tag: code=%d rows=%d requests=%d tag=%q", code, strings.Count(out, "machine_"), requested, tag)
	}
	if code, _, _ = runPolicyCLI(t, srv.URL, "machine", "list", "--limit", "500"); code == 0 {
		t.Fatal("--limit above 200 accepted")
	}
}

// PQ-04: the lists the server pages by offset read one bounded page and print the server's total instead of
// reading every page.
func TestOffsetListsShowOnePageAndTheServersTotal(t *testing.T) {
	type list struct {
		args        []string
		path, field string
		item        func(int) map[string]any
		noun, row   string
	}
	lists := []list{
		{[]string{"session", "shared"}, "/v1/terminal-sessions", "sessions", func(i int) map[string]any {
			return map[string]any{"id": fmt.Sprintf("session_%03d", i), "name": fmt.Sprintf("s%03d", i)}
		}, "sessions", "session_"},
		{[]string{"inbox", "requests"}, "/v1/team-inbox/requests", "requests", func(i int) map[string]any {
			return map[string]any{"request_id": fmt.Sprintf("request_%03d", i), "status": "pending"}
		}, "requests", "request_"},
		{[]string{"team", "invitations"}, "/v1/team-invitations", "items", func(i int) map[string]any {
			return map[string]any{"invitation_id": fmt.Sprintf("invitation_%03d", i), "team_id": "team_1", "state": "pending"}
		}, "invitations", "invitation_"},
		{[]string{"config", "repository", "list"}, "/v1/config-repositories", "items", func(i int) map[string]any {
			return map[string]any{"id": fmt.Sprintf("repository_%03d", i), "display_name": fmt.Sprintf("r%03d", i)}
		}, "repositories", "repository_"},
	}
	for _, l := range lists {
		t.Run(strings.Join(l.args, " "), func(t *testing.T) {
			requested := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != l.path {
					http.NotFound(w, r)
					return
				}
				requested++
				limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
				offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
				items := []map[string]any{}
				for i := offset; i < offset+limit && i < pagingTestTotal; i++ {
					items = append(items, l.item(i))
				}
				var next any
				if offset+len(items) < pagingTestTotal {
					next = offset + len(items)
				}
				writeAPIData(t, w, map[string]any{l.field: items, "pagination": map[string]any{"limit": limit, "offset": offset, "total": pagingTestTotal, "next_offset": next}})
			}))
			defer srv.Close()
			code, out, errOut := runPolicyCLI(t, srv.URL, append(l.args, "--limit", "20")...)
			rows := strings.Count(out, l.row)
			if code != 0 || requested != 1 || rows != 20 || !strings.Contains(errOut, "Showing 20 of 120 "+l.noun) {
				t.Fatalf("code=%d requests=%d rows=%d out=%q err=%q", code, requested, rows, out, errOut)
			}
			requested = 0
			if code, _, errOut = runPolicyCLI(t, srv.URL, append(l.args, "--all")...); code != 0 || requested != 1 || strings.Contains(errOut, "Showing") {
				t.Fatalf("--all: code=%d requests=%d err=%q", code, requested, errOut)
			}
		})
	}
}

// echoedLimit is the page limit a fake server echoes, as the real one does (the pager requires it).
func echoedLimit(r *http.Request) int {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	return limit
}
