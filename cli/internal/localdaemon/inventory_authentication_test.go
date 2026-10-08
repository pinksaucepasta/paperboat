package localdaemon

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestInventoryAuthenticationRequiresProvenRejection(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{config.ErrNoCredentials, true},
		{fmt.Errorf("credential unavailable: %w", config.ErrSecretNotFound), true},
		{api.ErrUnauthenticated, true},
		{&api.APIError{Status: 401}, true},
		{&api.APIError{Status: 403}, false},
		{errors.Join(api.ErrUnauthenticated, syscall.EIO), false},
		{errors.Join(&api.APIError{Status: 401}, syscall.EIO), false},
		{errors.Join(config.ErrSecretNotFound, context.DeadlineExceeded), false},
		{context.Canceled, false},
	} {
		if got := inventoryAuthenticationRequired(tc.err); got != tc.want {
			t.Fatalf("authentication classification for %T: got %v want %v", tc.err, got, tc.want)
		}
	}
}

func TestInventoryMixedAuthenticationFailureRetainsMachinesAndRecovers(t *testing.T) {
	mixed := errors.Join(api.ErrUnauthenticated, syscall.EIO)
	source := &scriptedMachineSource{results: []machineResult{
		{machines: []api.UserMachine{{ID: "machine_1", Alias: "before", InstallationGeneration: 1}}},
		{err: mixed},
		{machines: []api.UserMachine{{ID: "machine_1", Alias: "after", InstallationGeneration: 1}}},
		{err: api.ErrUnauthenticated},
	}}
	inventory, store := newTestInventory(t, source, time.Now)
	for _, want := range []struct{ state, alias string }{{"ready", "before"}, {"degraded", "before"}, {"ready", "after"}, {"awaiting_enrollment", ""}} {
		_ = inventory.Refresh(context.Background())
		snapshot, err := store.Snapshot(context.Background())
		if err != nil || snapshot.DaemonState != want.state {
			t.Fatal("inventory state did not preserve operational failure and recovery")
		}
		if want.alias == "" {
			if len(snapshot.Machines) != 0 {
				t.Fatal("proven authentication failure retained authorized inventory")
			}
		} else if len(snapshot.Machines) != 1 || snapshot.Machines[0].Alias != want.alias {
			t.Fatal("operational failure discarded the last usable inventory")
		}
	}
}
