//go:build darwin || linux

package runtime

import (
	"testing"
	"time"
)

// A computer that is reset or reinstalled enrolls again with the same SSH host keys while its old record still
// holds them (the old record is removed when the new runtime is verified, a minute later). The refusal must not
// hold the machine back for ten minutes: the first retry is 30 s, each later one twice as long, capped at ten minutes,
// and a published key change (the server removed the old record) retries at once. Before this, `pb ssh` to a
// machine that was reset stayed "Publishing the machine's SSH host keys" for 10 minutes (2026-11-12, X-41).
func TestRefusedHostKeysAreAskedAgainSoonAndThenLessOften(t *testing.T) {
	service := &managedSSHKeyReconciler{}
	for refusals, want := range map[uint32]time.Duration{1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute, 4: 4 * time.Minute, 5: 8 * time.Minute, 6: 10 * time.Minute, 40: 10 * time.Minute} {
		service.refusals.Store(refusals)
		if got := service.inUseDelay(); got != want {
			t.Fatalf("after %d refusals the retry waits %v, want %v", refusals, got, want)
		}
	}

	service.refusals.Store(1)
	service.hostKeyInUse.Store(true)
	refused := time.Now()
	service.refusedAt = refused
	if service.reconcileDue(refused.Add(10*time.Second), refused, false, false, false) {
		t.Fatal("asked again 10 s after a refusal")
	}
	if !service.reconcileDue(refused.Add(31*time.Second), refused, false, false, false) {
		t.Fatal("not asked again 31 s after the first refusal")
	}
	if !service.reconcileDue(refused.Add(time.Second), refused, false, true, false) {
		t.Fatal("a published key change did not retry a refused host at once")
	}
	service.refusals.Store(3)
	if service.reconcileDue(refused.Add(time.Minute), refused, false, false, false) || !service.reconcileDue(refused.Add(121*time.Second), refused, false, false, false) {
		t.Fatal("the third retry is not two minutes after the third refusal")
	}
}
