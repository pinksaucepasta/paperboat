//go:build darwin || linux

package session

import (
	"sync"
	"testing"

	"go.uber.org/goleak"
)

// A test that creates sessions and never closes them leaves their output and
// waiter goroutines behind; with -count=20 they pile up until the run times out.
func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var leakChecked sync.Map // *testing.T already registered

// registerLeakCheck fails the test, by name, if goroutines remain once all its other cleanups ran
// (the first call registers it, so it runs last).
func registerLeakCheck(t *testing.T) {
	if _, seen := leakChecked.LoadOrStore(t, true); seen {
		return
	}
	t.Cleanup(func() {
		leakChecked.Delete(t)
		goleak.VerifyNone(t, goleak.IgnoreAnyFunction("database/sql.(*DB).connectionOpener"))
	})
}
