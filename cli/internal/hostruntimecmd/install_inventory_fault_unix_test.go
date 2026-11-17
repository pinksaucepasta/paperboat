//go:build darwin || linux

package hostruntimecmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// canary-b 2026-11-17: dev-e723c114e over dev-b112b2a4a printed only "Paperboat could not finish the command" for a
// complete installation: the final inventory failed to look (systemctl show during the service restarts), a fault
// with no leftovers, which was returned as a bare join with no authored owner.
func TestInventoryFaultWithoutLeftoversIsAuthoredAndRetryable(t *testing.T) {
	var report bytes.Buffer
	_ = writePurgeResult(t.Context(), &report, withPurgeStage("final_inventory", errors.New("list Paperboat systemd units: exit status 1")))
	file := filepath.Join(t.TempDir(), "report")
	if err := os.WriteFile(file, report.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "/bin/sh", "-c", "cat "+file+"; exit 1")
	err := runInventoryCommand(t.Context(), command, "/bin/true")
	message, authored := FailureMessage(err)
	if !authored || !strings.Contains(message, "Paperboat is installed") || !strings.Contains(message, "pb install again") {
		t.Fatalf("inventory fault = %v (authored %t %q), want an authored sentence", err, authored, message)
	}
	if !errors.Is(err, errInventoryUnconfirmed) {
		t.Fatal("a fault without leftovers must be retryable")
	}
	var incomplete PurgeIncomplete
	if !errors.As(err, &incomplete) || len(incomplete.Stages) != 1 || incomplete.Stages[0] != "final_inventory" {
		t.Fatalf("the failing stage must stay inspectable: %+v", incomplete)
	}
}

func TestRetryInventoryRetriesOnlyAnInventoryThatCouldNotLook(t *testing.T) {
	inventoryRetryWait = 0
	t.Cleanup(func() { inventoryRetryWait = 2 * time.Second })
	unconfirmed := runtimeFailure("could not look", errInventoryUnconfirmed)
	leftovers := runtimeFailure("items remain")
	for _, test := range []struct {
		name    string
		results []error
		calls   int
		wantErr bool
	}{
		{"recovers", []error{unconfirmed, unconfirmed, nil}, 3, false},
		{"bounded", []error{unconfirmed, unconfirmed, unconfirmed, nil}, inventoryAttempts, true},
		{"leftovers are final", []error{leftovers, nil}, 1, true},
		{"clean", []error{nil}, 1, false},
	} {
		calls := 0
		err := retryInventory(t.Context(), func(context.Context) error { calls++; return test.results[calls-1] })
		if calls != test.calls || (err != nil) != test.wantErr {
			t.Errorf("%s: %d calls, err %v; want %d calls, error %t", test.name, calls, err, test.calls, test.wantErr)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := 0
	if err := retryInventory(ctx, func(context.Context) error { calls++; return unconfirmed }); err == nil || calls != 1 {
		t.Errorf("cancelled: %d calls, err %v; want one call and the error", calls, err)
	}
}
