package usermachines

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// Count, selection and durable records must describe one page even while another
// client deletes/restores a terminal. Runtime enrichment may be unavailable.
func TestTerminalInventoryConcurrentLifecyclePostgres(t *testing.T) {
	f := newWorkspaceMachineFixture(t)
	session, err := f.service.CreateTerminalSession(f.ctx, f.account, f.machine, "snapshot", "snapshot-"+f.suffix)
	if err != nil {
		t.Fatal(err)
	}
	writerCtx, cancel := context.WithCancel(f.ctx)
	done := make(chan error, 1)
	go func() {
		for i := 0; ; i++ {
			_, err := f.store.SQL().ExecContext(writerCtx, `UPDATE paperboat.user_machine_terminal_sessions SET deleted_at=CASE WHEN $2 THEN now() ELSE NULL END WHERE id=$1`, session.ID, i%2 == 0)
			if err != nil {
				done <- err
				return
			}
		}
	}()
	defer func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("lifecycle writer: %v", err)
		}
	}()
	for _, global := range []bool{false, true} {
		t.Run(fmt.Sprintf("global=%t", global), func(t *testing.T) {
			for i := 0; i < 256; i++ {
				var count, total int
				var err error
				if global {
					var page []SharedTerminalSession
					page, total, err = f.service.TerminalSessionInventory(f.ctx, f.account, InventoryOptions{Limit: 20})
					count = len(page)
				} else {
					var page []TerminalSession
					page, total, err = f.service.ListTerminalSessionInventory(f.ctx, f.account, f.machine, InventoryOptions{Limit: 20})
					count = len(page)
				}
				if err != nil || count != total {
					t.Fatalf("page %d: count=%d total=%d err=%v", i, count, total, err)
				}
			}
		})
	}
}
