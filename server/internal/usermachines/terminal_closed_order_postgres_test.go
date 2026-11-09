package usermachines

import (
	"testing"
	"time"
)

// Closed terminals list newest closed first (not by name) and carry closed_at; open ones stay by name.
func TestTerminalInventoryClosedSortedByClosedTimePostgres(t *testing.T) {
	f := newWorkspaceMachineFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	// Names sort a < b < c; closed times run the other way, and c has no closed_at (falls back to updated_at).
	ids := map[string]string{}
	for _, name := range []string{"a", "b", "c", "open"} {
		session, err := f.service.CreateTerminalSession(f.ctx, f.account, f.machine, name, "closed-order-"+name+"-"+f.suffix)
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = session.ID
	}
	closeAt := func(name string, closedAt, updatedAt any) {
		t.Helper()
		if _, err := f.store.SQL().ExecContext(f.ctx, `UPDATE paperboat.user_machine_terminal_sessions SET desired_state='closed',runtime_state='closed',closed_reason='user_closed',closed_at=$2,updated_at=$3 WHERE id=$1`, ids[name], closedAt, updatedAt); err != nil {
			t.Fatal(err)
		}
	}
	closeAt("a", now.Add(-3*time.Hour), now.Add(-3*time.Hour))
	closeAt("b", now.Add(-1*time.Hour), now.Add(-1*time.Hour))
	closeAt("c", nil, now.Add(-2*time.Hour))
	closed := true
	for _, live := range []bool{false, true} {
		page, total, err := f.service.TerminalSessionInventory(f.ctx, f.account, InventoryOptions{Limit: 20, Closed: &closed, MachineID: f.machine, WithoutLiveMetadata: !live})
		if err != nil || total != 3 || len(page) != 3 {
			t.Fatalf("live=%t closed page %+v total %d err %v", live, page, total, err)
		}
		for i, want := range []string{"b", "c", "a"} {
			if page[i].ID != ids[want] {
				t.Fatalf("live=%t position %d is %s, want %s (newest closed first)", live, i, page[i].Name, want)
			}
		}
		if page[0].ClosedAt == nil || !page[0].ClosedAt.Equal(now.Add(-time.Hour)) || page[1].ClosedAt != nil {
			t.Fatalf("live=%t closed_at b=%v c=%v", live, page[0].ClosedAt, page[1].ClosedAt)
		}
	}
	// The second page continues the same order.
	second, _, err := f.service.TerminalSessionInventory(f.ctx, f.account, InventoryOptions{Limit: 1, Offset: 1, State: "closed", MachineID: f.machine, WithoutLiveMetadata: true})
	if err != nil || len(second) != 1 || second[0].ID != ids["c"] {
		t.Fatalf("second page %+v err %v", second, err)
	}
	open := false
	page, _, err := f.service.TerminalSessionInventory(f.ctx, f.account, InventoryOptions{Limit: 20, Closed: &open, MachineID: f.machine, WithoutLiveMetadata: true})
	if err != nil || len(page) == 0 {
		t.Fatalf("open page %+v err %v", page, err)
	}
	for i := 1; i < len(page); i++ {
		if page[i-1].Name > page[i].Name {
			t.Fatalf("open sessions not by name: %q before %q", page[i-1].Name, page[i].Name)
		}
	}
}
