package controlplane

import (
	"strconv"
	"testing"
)

// F21-168: an edge's route snapshot is paged; the cursor names the first item of a page, the pages cover the snapshot once
// in order, and a cursor that is not one of its positions is refused.
func TestRouteSnapshotPagesCoverTheSnapshotOnceInOrder(t *testing.T) {
	items := make([]map[string]any, 2*routeSnapshotPageSize+17)
	for i := range items {
		items[i] = map[string]any{"i": i}
	}
	var got []int
	cursor, pages := "", 0
	for {
		page, next, err := routeSnapshotPage(items, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) > routeSnapshotPageSize {
			t.Fatalf("page of %d items", len(page))
		}
		for _, item := range page {
			got = append(got, item["i"].(int))
		}
		pages++
		if next == "" {
			break
		}
		cursor = next
	}
	if len(got) != len(items) || pages != 3 {
		t.Fatalf("%d items in %d pages, want %d in 3", len(got), pages, len(items))
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("item %d is %d", i, v)
		}
	}
	for _, bad := range []string{"-1", "x", strconv.Itoa(len(items) + 1)} {
		if _, _, err := routeSnapshotPage(items, bad); err == nil {
			t.Fatalf("cursor %q accepted", bad)
		}
	}
	if page, next, err := routeSnapshotPage(nil, ""); err != nil || len(page) != 0 || next != "" {
		t.Fatalf("empty snapshot: %v %q %v", page, next, err)
	}
}
