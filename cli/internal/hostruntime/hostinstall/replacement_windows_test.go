//go:build windows

package hostinstall

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWindowsReplacementRejectsMalformedNativeSnapshotBeforeMutation(t *testing.T) {
	layout, err := service.WindowsUserLayout("S-1-5-21-111-222-333-1001")
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []string{"", "null", `{}`, `{"unknown":true}`, `{} {}`, `{"installation_absent":false}`, `{"installation_absent":true,"source":{}}`, `{"installation_absent":false,"config":{},"source":{}}`, strings.Repeat("x", (128<<10)+1)} {
		if err := (windowsInstallationState{layout: layout}).Restore(context.Background(), []byte(snapshot)); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("invalid snapshot error = %v", err)
		}
	}
}

// First enrollment journals an absent baseline: no predecessor, never a corrupt record. The updater service read
// it as ErrInvalidRequest and exited, so enrollment on Windows failed at "install services".
func TestWindowsCapturedSourceOfAnAbsentInstallationIsNotExist(t *testing.T) {
	body, err := json.Marshal(windowsInstallationSnapshot{InstallationAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := windowsCapturedSourceProjection(body, ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent baseline error = %v, want os.ErrNotExist", err)
	}
	for _, corrupt := range []string{"", "{}", `{"installation_absent":false}`, `{"installation_absent":true,"source":{}}`, "x"} {
		if _, err := windowsCapturedSourceProjection([]byte(corrupt), ""); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("corrupt snapshot %q error = %v, want ErrInvalidRequest", corrupt, err)
		}
	}
}

// victus: after a failed update left hostd stopped, `pb install` of a good build failed with "hostd lifecycle pipe
// connection failed" and could never recover the machine. A stopped hostd has nothing to admit.
func TestWindowsReplacementSkipsMaintenanceAdmissionWhenHostdIsStopped(t *testing.T) {
	layout, err := service.WindowsUserLayout("S-1-5-21-111-222-333-1001")
	if err != nil {
		t.Fatal(err)
	}
	restore := windowsHostdRunning
	t.Cleanup(func() { windowsHostdRunning = restore })
	windowsHostdRunning = func(context.Context, service.Layout) (bool, error) { return false, nil }
	abort, err := prepareWindowsReplacementMaintenance(context.Background(), layout, `C:\does-not-exist\hostd.token`, false, time.Second)
	if err != nil {
		t.Fatalf("stopped hostd still required maintenance admission: %v", err)
	}
	if err := abort(); err != nil {
		t.Fatal(err)
	}
	windowsHostdRunning = func(context.Context, service.Layout) (bool, error) { return true, nil }
	if _, err := prepareWindowsReplacementMaintenance(context.Background(), layout, `C:\does-not-exist\hostd.token`, false, time.Second); err == nil {
		t.Fatal("a running hostd was not asked for maintenance admission")
	}
}
