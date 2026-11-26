//go:build darwin || linux

package hostinstall

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/updated"
)

// heldActivation holds the activation lock of a throwaway account's install paths (nobody), as the activation helper
// does through an update's stability window. Directories it created are removed afterwards.
func heldActivation(t *testing.T) (installPaths, *user.User) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root: the activation lock lives in root-owned state")
	}
	account, err := user.Lookup("nobody")
	if err != nil {
		t.Skip("no nobody account")
	}
	uid, _ := strconv.Atoi(account.Uid)
	paths := platformPaths(uid)
	for _, directory := range []string{filepath.Dir(paths.metadata), paths.updateState} {
		if _, err := os.Lstat(directory); errors.Is(err, os.ErrNotExist) {
			t.Cleanup(func() { _ = os.RemoveAll(directory) })
		}
	}
	if err := secureRootDirectory(paths.updateState, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := updated.LockUnixNativeInstall(context.Background(), paths.updateState)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	return paths, account
}

// An explicit update while an activation holds the lock refuses at once as busy; it waited up to 15 minutes
// (smoke 11-26, removed machine .8 to .9).
func TestExplicitUpdateRefusesAtOnceWhileAnActivationHoldsTheLock(t *testing.T) {
	_, account := heldActivation(t)
	t.Setenv("PAPERBOAT_INVOKING_UID", account.Uid)
	uid, _ := strconv.Atoi(account.Uid)
	gid, _ := strconv.Atoi(account.Gid)
	group, err := user.LookupGroupId(account.Gid)
	if err != nil {
		t.Fatal(err)
	}
	request := Request{Schema: SchemaV1, Platform: runtime.GOOS, User: account.Username, UID: uid, GID: gid, Group: group.Name, Home: account.HomeDir,
		EnrollmentPending: true, UpdateBundle: filepath.Join(t.TempDir(), "release.pb-update"), SuppliedUpdate: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	err = SuppliedBinary(ctx, request, "install-supplied", PreparedUpdateHooks{})
	if !errors.Is(err, hostdproto.ErrMaintenanceBusy) || time.Since(started) > 2*time.Second {
		t.Fatalf("explicit update during an activation = %v after %s, want ErrMaintenanceBusy at once", err, time.Since(started))
	}
}

// The periodic carrier reconcile skips a pass while an activation holds the lock; waiting hit the 2-minute pass bound
// and logged service_failed deadline_exceeded every pass (smoke 11-26).
func TestCarrierReconcileSkipsWhileAnActivationHoldsTheLock(t *testing.T) {
	paths, _ := heldActivation(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := reconcileInstalledCarrier(ctx, paths.instance); err != nil || ctx.Err() != nil {
		t.Fatalf("reconcile during an activation = %v (context %v), want a skipped pass", err, ctx.Err())
	}
}
