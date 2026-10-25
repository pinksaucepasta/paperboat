package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// PQ-04: pb machine list shows one bounded page with the server's total, never every machine.
func TestMachineListShowsOnePageAndTheServersTotal(t *testing.T) {
	requested := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/machines" {
			http.NotFound(w, r)
			return
		}
		requested++
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		items := []map[string]any{}
		for i := offset; i < offset+limit && i < 120; i++ {
			items = append(items, map[string]any{"id": fmt.Sprintf("machine_%03d", i), "alias": fmt.Sprintf("m%03d", i), "state": "online"})
		}
		var next any
		if offset+len(items) < 120 {
			next = offset + len(items)
		}
		writeAPIData(t, w, map[string]any{"items": items, "pagination": map[string]any{"limit": limit, "offset": offset, "total": 120, "next_offset": next}})
	}))
	defer srv.Close()
	code, out, errOut := runPolicyCLI(t, srv.URL, "machine", "list")
	if code != 0 || strings.Count(out, "machine_") != defaultListLimit || requested != 1 || !strings.Contains(errOut, "Showing 50 of 120 machines") {
		t.Fatalf("code=%d rows=%d requests=%d err=%q", code, strings.Count(out, "machine_"), requested, errOut)
	}
	requested = 0
	code, out, _ = runPolicyCLI(t, srv.URL, "machine", "list", "--all")
	if code != 0 || strings.Count(out, "machine_") != 120 {
		t.Fatalf("--all: code=%d rows=%d", code, strings.Count(out, "machine_"))
	}
	if code, _, _ = runPolicyCLI(t, srv.URL, "machine", "list", "--limit", "500"); code == 0 {
		t.Fatal("--limit above 200 accepted")
	}
}
