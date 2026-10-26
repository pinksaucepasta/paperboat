package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/inboxdispatch"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

type fakeInboxMachines []usermachines.InboxMachine

func (f fakeInboxMachines) InboxMachines(_ context.Context, _, after, only string, limit int) ([]usermachines.InboxMachine, bool, error) {
	var out []usermachines.InboxMachine
	for _, m := range f {
		if (only == "" || m.ID == only) && m.ID > after {
			out = append(out, m)
		}
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

type fakeLiveInbox struct {
	calls atomic.Int64
	files map[string][]inboxdispatch.Item
}

func (f *fakeLiveInbox) List(_ context.Context, request inboxdispatch.Request) (inboxdispatch.Listing, error) {
	f.calls.Add(1)
	if request.MachineID == "m4" {
		return inboxdispatch.Listing{}, inboxdispatch.ErrMachineOutdated
	}
	items := f.files[request.MachineID]
	return inboxdispatch.Listing{Total: len(items), Items: items}, nil
}
func (f *fakeLiveInbox) Open(context.Context, inboxdispatch.Request) (inboxdispatch.File, error) {
	return inboxdispatch.File{}, nil
}
func (f *fakeLiveInbox) Delete(context.Context, inboxdispatch.Request) error { return nil }

// The workspace Inbox asks at most the policy's number of machines per request, in machine order, and gives a
// machine cursor for the next batch: seven machines with a bound of three take three requests. Files are merged
// newest first; machines that cannot answer are listed with the reason, without asking an offline one; a
// repeated request inside the cache window asks no machine again.
func TestWorkspaceInboxListIsBoundedPerRequest(t *testing.T) {
	base := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	machines := fakeInboxMachines{}
	live := &fakeLiveInbox{files: map[string][]inboxdispatch.Item{}}
	for i := 1; i <= 7; i++ {
		id := fmt.Sprintf("m%d", i)
		machines = append(machines, usermachines.InboxMachine{ID: id, Alias: "host-" + id, State: "online", Online: i != 2, Receives: i != 3})
		live.files[id] = []inboxdispatch.Item{{ID: fmt.Sprintf("%016x", i), Name: fmt.Sprintf("report-%d.pdf", i), Size: int64(i), ReceivedAt: base.Add(time.Duration(i) * time.Minute)}}
	}
	handler := workspaceInboxList(machines, live, config.InboxListPolicy{Machines: 3, Budget: time.Second, CacheFor: time.Minute}, func() time.Time { return base })
	get := func(query string) map[string]any {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/v1/inbox?"+query, nil)
		r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, principal{User: auth.User{ID: "acct"}}))
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /v1/inbox?%s: %d %s", query, w.Code, w.Body.String())
		}
		var out struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Data
	}
	names := func(data map[string]any) []string {
		var out []string
		for _, item := range data["items"].([]any) {
			out = append(out, item.(map[string]any)["name"].(string))
		}
		return out
	}
	reasons := func(data map[string]any) []string {
		var out []string
		for _, u := range data["unavailable"].([]any) {
			out = append(out, fmt.Sprintf("%s:%s", u.(map[string]any)["machine_id"], u.(map[string]any)["reason"]))
		}
		sort.Strings(out)
		return out
	}
	var all []string
	var unavailable []string
	pages, cursor := 0, ""
	for {
		query := ""
		if cursor != "" {
			query = "machine_cursor=" + cursor
		}
		data := get(query)
		pages++
		if checked := data["machines_checked"].(float64); checked > 3 {
			t.Fatalf("page %d checked %v machines, bound is 3", pages, checked)
		}
		all = append(all, names(data)...)
		unavailable = append(unavailable, reasons(data)...)
		next, more := data["next_machine_cursor"].(string)
		if !more {
			break
		}
		cursor = next
	}
	if pages != 3 {
		t.Fatalf("seven machines with a bound of three took %d requests, want 3", pages)
	}
	if want := []string{"report-1.pdf", "report-5.pdf", "report-6.pdf", "report-7.pdf"}; fmt.Sprint(sortedCopy(all)) != fmt.Sprint(want) {
		t.Fatalf("files %v, want %v", all, want)
	}
	if want := []string{"m2:machine_offline", "m3:inbox_unavailable", "m4:machine_update_required"}; fmt.Sprint(unavailable) != fmt.Sprint(want) {
		t.Fatalf("unavailable %v, want %v", unavailable, want)
	}
	if calls := live.calls.Load(); calls != 5 {
		t.Fatalf("asked machines %d times, want 5 (the offline and non-receiving machines are not asked)", calls)
	}
	// The first batch: newest first, and its totals are of its own machines.
	first := get("")
	if fmt.Sprint(names(first)) != "[report-1.pdf]" || first["files_on_checked_machines"].(float64) != 1 {
		t.Fatalf("first batch %v, files on checked machines %v", names(first), first["files_on_checked_machines"])
	}
	if calls := live.calls.Load(); calls != 5 {
		t.Fatalf("a repeated request inside the cache window asked the machines again (%d calls)", calls)
	}
	third := get("machine_cursor=" + encodeTestCursor("m6"))
	if fmt.Sprint(names(third)) != "[report-7.pdf]" {
		t.Fatalf("after m6: %v", names(third))
	}
	if got := names(get("machine_id=m6&q=REPORT-6")); fmt.Sprint(got) != "[report-6.pdf]" {
		t.Fatalf("one machine, q: %v", got)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func encodeTestCursor(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}
