package usermachines

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A session the machine started while Paperboat was unreachable is registered once the machine is online, is listed
// like any other, and is ended the way any other is (F21-11, L493).
func TestPostgresOfflineTerminalSessionIsAdoptedOnceTheMachineIsOnline(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	service, host, owner, machine := terminalOrphanFixture(t, store)
	offline := func() string { return "terminal_offline_" + strings.ReplaceAll(uuid.NewString(), "-", "_") }
	first, second, unknown := offline(), offline(), offline()
	host.has = map[string]bool{first: true, second: true}
	host.names = map[string]string{first: "work", second: "work"}
	listed := func(id string) bool {
		t.Helper()
		items, err := service.TerminalSessionCatalog(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			if item.ID == id {
				return true
			}
		}
		return false
	}

	if _, err := service.AdoptOfflineTerminalSession(ctx, owner, machine, "terminal_"+uuid.NewString()); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("an id that is not an offline session id = %v, want not found", err)
	}
	if _, err := service.AdoptOfflineTerminalSession(ctx, owner, machine, unknown); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("a session the host does not have = %v, want not found", err)
	}
	if listed(unknown) {
		t.Fatal("a session the host does not have was left registered")
	}
	// An account without terminal access to the machine, and a machine that is not the caller's, are refused without
	// asking the host or leaving a row.
	asked := host.askedCount(first)
	if _, err := service.AdoptOfflineTerminalSession(ctx, "ti_stranger_"+first, machine, first); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an account without access to the machine = %v, want ErrNotFound", err)
	}
	if _, err := service.AdoptOfflineTerminalSession(ctx, owner, "ti_machine_other_"+first, first); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a machine that is not the caller's = %v, want ErrNotFound", err)
	}
	if host.askedCount(first) != asked || listed(first) {
		t.Fatalf("refused calls asked the host (%d -> %d) or left the session listed=%v", asked, host.askedCount(first), listed(first))
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET online=false WHERE id=$1`, machine); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AdoptOfflineTerminalSession(ctx, owner, machine, first); !errors.Is(err, ErrMachineOffline) || host.askedCount(first) != 0 || listed(first) {
		t.Fatalf("offline machine: err=%v asked=%d listed=%v, want ErrMachineOffline, not asked, not listed", err, host.askedCount(first), listed(first))
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET online=true WHERE id=$1`, machine); err != nil {
		t.Fatal(err)
	}

	got, err := service.AdoptOfflineTerminalSession(ctx, owner, machine, first)
	if err != nil || got.ID != first || got.Name != "work" || !listed(first) {
		t.Fatalf("adopt = %+v err=%v listed=%v, want the host's session named work, listed", got, err, listed(first))
	}
	asked := host.askedCount(first) // listing may ask too; adopting again must not
	again, err := service.AdoptOfflineTerminalSession(ctx, owner, machine, first)
	if err != nil || again.ID != first || host.askedCount(first) != asked {
		t.Fatalf("adopt again = %+v err=%v asked %d -> %d, want the same session without asking the host again", again, err, asked, host.askedCount(first))
	}
	other, err := service.AdoptOfflineTerminalSession(ctx, owner, machine, second)
	if err != nil || other.ID != second || other.Name == "work" || other.Name == "" {
		t.Fatalf("second session with the same host name = %+v err=%v, want it adopted under another name", other, err)
	}

	if _, err := service.CloseTerminalSession(ctx, owner, machine, first); err != nil {
		t.Fatalf("closing an adopted session: %v", err)
	}
	var desired string
	if err := store.SQL().QueryRowContext(ctx, `SELECT desired_state FROM paperboat.user_machine_terminal_sessions WHERE id=$1`, first).Scan(&desired); err != nil || desired != "closed" {
		t.Fatalf("adopted session after close: desired_state=%q err=%v, want closed", desired, err)
	}
}
