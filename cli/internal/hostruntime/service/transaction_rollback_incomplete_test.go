package service

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/shorttemp"
)

type unrestorableInstallation struct{ fileInstallationState }

var errBaselineCarrierDown = errors.New("the previous carrier did not start again")

func (unrestorableInstallation) Restore(context.Context, []byte) error { return errBaselineCarrierDown }

// A failed replacement whose rollback does not finish must say so: a revoked test machine's install reported "the
// previous version was restored" while the new hostd, worker and daemon kept running (dev-76b5e9764 over dev-080c53cc9).
func TestLifecycleReplaceNamesARollbackThatDidNotFinish(t *testing.T) {
	for _, restorable := range []bool{true, false} {
		root := shorttemp.MachineDir(t)
		image := filepath.Join(root, "image")
		if err := writeFile(image, "prior image"); err != nil {
			t.Fatal(err)
		}
		var installation NativeInstallationState = fileInstallationState{image}
		if !restorable {
			installation = unrestorableInstallation{fileInstallationState{image}}
		}
		var calls []string
		var mu sync.Mutex
		component := newFakeTransactionalComponent("hostd", &calls, &mu)
		manager, err := NewLifecycleManager(LifecycleConfig{StateRoot: filepath.Join(root, "lifecycle"), NativeInstallation: installation, Components: []TransactionalComponent{component}})
		if err != nil {
			t.Fatal(err)
		}
		failure := errors.New("the new version did not become ready")
		err = manager.Replace(context.Background(), func(context.Context) error { return failure })
		if !errors.Is(err, failure) {
			t.Fatalf("restorable=%v: replace error = %v", restorable, err)
		}
		if incomplete := errors.Is(err, ErrLifecycleRollbackIncomplete); incomplete == restorable {
			t.Fatalf("restorable=%v: rollback reported incomplete=%v: %v", restorable, incomplete, err)
		}
		if !restorable && !errors.Is(err, errBaselineCarrierDown) {
			t.Fatalf("the rollback's own cause is lost: %v", err)
		}
	}
}
