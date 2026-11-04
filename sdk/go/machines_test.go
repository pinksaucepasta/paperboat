package paperboat

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
)

func TestSDKMachineResolutionAndPagination(t *testing.T) {
	f := newSDKServer(t)
	var mu sync.Mutex
	renamed := false
	pages := 0
	f.inventory = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("workspace") != "team-test" || r.URL.Query().Get("sort") != "alias" || r.URL.Query().Get("limit") != "200" {
			t.Error("inventory query changed")
		}
		offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
		if err != nil || offset < 0 || offset > 2 {
			t.Error("invalid pagination request")
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		pages++
		names := []string{"duplicate", "duplicate", "unique"}
		if renamed {
			names = []string{"unique", "duplicate", "renamed"}
		}
		name := names[offset]
		mu.Unlock()
		ids := []string{"machine-a", "machine-b", "machine-c"}
		var next *int
		if offset < 2 {
			value := offset + 1
			next = &value
		}
		writeInventory(w, r, []map[string]any{{"id": ids[offset], "alias": name, "online": true, "future_field": true}}, 3, next)
	}
	options := f.options()
	options.Workspace = "team-test"
	c := newClient(t, options)
	page, err := c.Machines(context.Background())
	if err != nil || len(page.Items) != 3 || page.Total != 3 {
		t.Fatalf("inventory size=%d total=%d err=%v", len(page.Items), page.Total, err)
	}
	mu.Lock()
	count := pages
	mu.Unlock()
	if count != 3 {
		t.Fatalf("pages=%d", count)
	}
	byID, err := c.Machine(context.Background(), "machine-b")
	if err != nil || byID.ID != "machine-b" {
		t.Fatal("exact ID did not resolve")
	}
	_, err = c.Machine(context.Background(), "duplicate")
	var ambiguous *AmbiguousMachineError
	if !errors.As(err, &ambiguous) || ambiguous.Name != "duplicate" {
		t.Fatalf("ambiguous name error: %v", err)
	}
	for _, target := range []string{"inaccessible-machine", "Unique"} {
		if _, err = c.Machine(context.Background(), target); !errors.Is(err, ErrMachineNotFound) {
			t.Fatalf("inaccessible or non-exact name: %v", err)
		}
	}
	pinned, err := c.Machine(context.Background(), "unique")
	if err != nil || pinned.ID != "machine-c" {
		t.Fatal("unique name did not resolve")
	}
	// A later lookup/retry uses this resolved ID, even after another machine takes the old name.
	mu.Lock()
	renamed = true
	mu.Unlock()
	retried, err := c.Machine(context.Background(), pinned.ID)
	if err != nil || retried.ID != pinned.ID || retried.Name != "renamed" {
		t.Fatal("resolved identity changed after rename")
	}
	current, err := c.Machine(context.Background(), "unique")
	if err != nil || current.ID == pinned.ID {
		t.Fatal("rename fixture did not move the alias")
	}
}
