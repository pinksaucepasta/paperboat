//go:build darwin || linux

package hostruntimecmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
)

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// ubuntu24, 2026-10-26: the updater's heal ran while a failed update was restoring the guard; the undo
// then stopped the healed guard and left none running. The heal runs only while it holds the lock.
func TestGuardHealWaitsForTheInstallationLock(t *testing.T) {
	oldLock, oldHeal, oldWait, oldInventory := lockInstallation, healGuard, healRetryWait, settleInventory
	t.Cleanup(func() {
		lockInstallation, healGuard, healRetryWait, settleInventory = oldLock, oldHeal, oldWait, oldInventory
	})
	healRetryWait = time.Millisecond
	held, locked, healedWhileUnlocked := 2, false, false
	lockInstallation = func(context.Context, string) (io.Closer, error) {
		if held > 0 {
			held--
			return nil, errors.New("activation pending")
		}
		locked = true
		return closerFunc(func() error { locked = false; return nil }), nil
	}
	healGuard = func(context.Context, installsource.Source, string) (bool, error) {
		healedWhileUnlocked = !locked
		return true, nil
	}
	inventoriedWhileLocked := false
	settleInventory = func(_ context.Context, uid int, check bool) error {
		inventoriedWhileLocked = locked && uid == 1001 && !check
		return errors.New("items of another Paperboat version remain: process 7 (/old)")
	}
	var log bytes.Buffer
	healMachineGuard(t.Context(), "/state", 1001, installsource.Source{Version: "dev-test"}, "/bin/pb", &log)
	if healedWhileUnlocked || locked || held != 0 || !inventoriedWhileLocked || !strings.Contains(log.String(), "replaced it with dev-test") || !strings.Contains(log.String(), "process 7 (/old)") {
		t.Fatalf("healedWhileUnlocked=%v stillLocked=%v pendingAttempts=%d inventoried=%v log=%q", healedWhileUnlocked, locked, held, inventoriedWhileLocked, log.String())
	}
}
