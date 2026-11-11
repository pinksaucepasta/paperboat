//go:build darwin || linux || windows

package hostruntimecmd

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
)

// During a Windows update the new hostd's worker reconciles managed SSH while the previous generation's carrier
// still serves: reconcile returns at once without an error, and the latest wanted state is applied once the carrier
// of this hostd generation serves. A paired carrier is applied to directly, and a carrier that never pairs ends the
// wait at its bound without an error to the caller.
func TestCarrierSSHWaitsForThePairedCarrierWithoutAnError(t *testing.T) {
	var mu sync.Mutex
	endpoint, applied := "old", []bool(nil)
	ssh := &carrierSSH{
		read: func(context.Context) (hostinstall.CarrierConfig, error) {
			mu.Lock()
			defer mu.Unlock()
			return hostinstall.CarrierConfig{HostdEndpoint: endpoint}, nil
		},
		paired: func(config hostinstall.CarrierConfig) bool { return config.HostdEndpoint == "new" },
		apply: func(_ context.Context, config hostinstall.CarrierConfig, enabled bool) error {
			if config.HostdEndpoint != "new" {
				return errors.New("applied to the previous generation's carrier, which fences this worker")
			}
			mu.Lock()
			applied = append(applied, enabled)
			mu.Unlock()
			return nil
		},
	}
	started := time.Now()
	if err := ssh.reconcile(context.Background(), true); err != nil {
		t.Fatalf("reconcile during the update = %v, want no error", err)
	}
	if err := ssh.reconcile(context.Background(), false); err != nil {
		t.Fatalf("second reconcile during the update = %v, want no error", err)
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("reconcile waited for the carrier switch; it runs inside worker startup")
	}
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	endpoint = "new" // the network moved to this generation's carrier
	mu.Unlock()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		mu.Lock()
		got := append([]bool(nil), applied...)
		mu.Unlock()
		if len(got) == 1 && !got[0] {
			break
		}
		if len(got) > 1 || time.Now().After(deadline) {
			t.Fatalf("applied %v once the carrier paired, want only the latest wanted state [false]", got)
		}
	}
	if err := ssh.reconcile(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(applied) != 2 || !applied[1] {
		t.Fatalf("a paired carrier was not applied to directly: %v", applied)
	}
}

func TestCarrierSSHGivesUpQuietlyWhenTheCarrierNeverPairs(t *testing.T) {
	wait := carrierSSHPairWait
	carrierSSHPairWait = 200 * time.Millisecond
	t.Cleanup(func() { carrierSSHPairWait = wait })
	applies := 0
	ssh := &carrierSSH{
		read:   func(context.Context) (hostinstall.CarrierConfig, error) { return hostinstall.CarrierConfig{}, nil },
		paired: func(hostinstall.CarrierConfig) bool { return false },
		apply:  func(context.Context, hostinstall.CarrierConfig, bool) error { applies++; return nil },
	}
	if err := ssh.reconcile(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		ssh.mu.Lock()
		waiting := ssh.waiting
		ssh.mu.Unlock()
		if !waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the wait did not end at its bound")
		}
	}
	if applies != 0 {
		t.Fatalf("applied %d times to a carrier that never paired", applies)
	}
}
