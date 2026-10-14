package httpapi

import (
	"net/http/httptest"
	"testing"
)

func TestInventoryQueryRejectsAmbiguousAndUnsupportedFilters(t *testing.T) {
	for _, query := range []string{"limit=0", "limit=201", "limit=10&limit=20", "offset=-1", "q=a&q=b", "state=unknown", "owner=a%20b", "closed=1", "cursor=ignored"} {
		t.Run(query, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/v1/machines?"+query, nil)
			if _, ok := readInventoryQuery(w, r, []string{"online"}, true); ok || w.Code != 400 {
				t.Fatalf("invalid query accepted status=%d", w.Code)
			}
		})
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/machines?limit=200&offset=200&q=project&state=online&owner=shared&closed=false", nil)
	out, ok := readInventoryQuery(w, r, []string{"online"}, true)
	if !ok || out.Limit != 200 || out.Offset != 200 || out.Q != "project" || out.Owner != "shared" || out.Closed == nil || *out.Closed {
		t.Fatalf("effective query = %#v", out)
	}
	page := inventoryPagination(200, 200, 450, 200)
	if page["next_offset"] != 400 || page["limit"] != 200 {
		t.Fatalf("page=%v", page)
	}
}
func TestCursorInventoryQueryPreservesCursorAndRejectsOffset(t *testing.T) {
	for _, query := range []string{"cursor=a&cursor=b", "cursor=a&offset=0"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/v1/previews?"+query, nil)
		if _, ok := readCursorInventoryQuery(w, r, nil); ok || w.Code != 400 {
			t.Fatal("invalid cursor query accepted")
		}
	}
	r := httptest.NewRequest("GET", "/v1/previews?cursor=signed&state=active", nil)
	out, ok := readCursorInventoryQuery(httptest.NewRecorder(), r, []string{"active"})
	if !ok || out.State != "active" || r.URL.Query().Get("cursor") != "signed" {
		t.Fatal("cursor/filter lost")
	}
}
