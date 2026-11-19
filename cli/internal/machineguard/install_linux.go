//go:build linux

package machineguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/cli/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/cli/internal/diagnostics"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Install copies the invoked executable into root-only installation storage and
// restores only address isolation before user sessions. DNS startup does not gate
// normal logins. No user-writable binary runs as root.
func installGuard(ctx context.Context, executable, browserDomain string, units map[string]string, undo **Undo) (resultErr error) {
	defer diagnostics.UpdateStep("guard_install")()
	if err := requirePrivilege(); err != nil {
		return err
	}
	lifecycle, err := lockGuardLifecycle(DefaultStateDir)
	if err != nil {
		return err
	}
	defer lifecycle.Close()
	if err := recordInstallBrowserDomain(browserDomain); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}

	if os.Geteuid() != 0 {
		return errors.New("installing the machine guard requires root")
	}
	for _, name := range []string{"paperboat-machineguard-deny.service", "paperboat-machineguard.service"} {
		if err := validateGuardUnit(filepath.Join("/etc/systemd/system", name)); err != nil {
			return err
		}
	}
	// NSS certificate tools serve browser trust only. Their absence leaves trust
	// pending instead of stopping the guard installation.
	trustErr := trustPending(ctx, ensureLocalTrustTools(ctx))
	if trustErr != nil && !isTrustPending(trustErr) {
		return trustErr
	}
	directory := "/usr/local/libexec/paperboat-machineguard"
	if err := protectedDirectory(directory, 0755); err != nil {
		return err
	}
	rollback, err := snapshotGuardFiles(filepath.Join(directory, "pb"), "/etc/systemd/system/paperboat-machineguard.service", "/etc/systemd/system/paperboat-machineguard-deny.service")
	if err != nil {
		return err
	}
	wasRunning := false
	candidateStarted := false
	unitExisted := false
	defer finishGuardInstall(ctx, rollback, &resultErr, undo, func(c context.Context) error {
		if !candidateStarted {
			return nil
		}
		err := exec.CommandContext(c, "systemctl", "stop", "paperboat-machineguard.service").Run()
		if !unitExisted { // the rollback deletes the unit file, so its descriptor store must go first
			releaseGuardFDStore(c, execGuardCommand, "paperboat-machineguard.service")
		}
		return err
	}, func(c context.Context) error {
		if err := exec.CommandContext(c, "systemctl", "daemon-reload").Run(); err != nil {
			return err
		}
		if wasRunning {
			return exec.CommandContext(c, "systemctl", "start", "paperboat-machineguard.service").Run()
		}
		return nil
	})
	if _, err := os.Lstat("/etc/systemd/system/paperboat-machineguard.service"); err == nil {
		unitExisted = true
		wasRunning = exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", "paperboat-machineguard.service").Run() == nil
		if output, err := exec.CommandContext(ctx, "systemctl", "stop", "paperboat-machineguard.service").CombinedOutput(); err != nil {
			return fmt.Errorf("stop owned guard before local CA maintenance: %w: %s", err, output)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	if err := PrepareLocalCARenewal(ctx, Config{StateDir: DefaultStateDir}); err != nil {
		return fmt.Errorf("renew local browser trust: %w", err)
	}
	if err := prepareInstalledLocalCA(ctx, Config{StateDir: DefaultStateDir}); err != nil {
		if !isTrustPending(err) {
			return err
		}
		trustErr = errors.Join(trustErr, err)
	}
	endCopy := diagnostics.UpdateStep("guard_candidate_copy_sync")
	defer func() { endCopy() }()
	source, err := os.Open(executable)
	if err != nil {
		return err
	}
	defer source.Close()
	//paperboat:allow-source-policy atomic-replacement owner=machineguard reason=streamed-guard-binary-staging
	target, err := os.CreateTemp(directory, ".pb-")
	if err != nil {
		return err
	}
	temporary := target.Name()
	defer os.Remove(temporary)
	if _, err = atomicfile.SyncedCopy(target, source); err == nil {
		err = target.Chmod(0755)
	}
	if err == nil {
		err = target.Sync()
	}
	err = errors.Join(err, target.Close())
	if err != nil {
		return err
	}
	endCopy()
	endCopy = func() {}
	//paperboat:allow-source-policy atomic-replacement owner=machineguard reason=streamed-guard-binary-staging
	err = os.Rename(temporary, filepath.Join(directory, "pb"))
	rollback.record(filepath.Join(directory, "pb"))
	if err != nil {
		return err
	}
	if units == nil {
		units = Units()
	}
	if err := checkGuardUnits(units, "paperboat-machineguard-deny.service", "paperboat-machineguard.service"); err != nil {
		return err
	}
	for name, unit := range units {
		path := filepath.Join("/etc/systemd/system", name)
		err = writeGuardUnit(path, unit)
		rollback.record(path)
		if err != nil {
			return err
		}
	}
	for _, args := range guardInstallCommands {
		if args[0] == "restart" {
			candidateStarted = true
		}
		if output, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("machine guard service: %w: %s", err, output)
		}
	}
	return trustErr
}

func execGuardCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// releaseGuardFDStore empties the stopped guard's descriptor store. Call it before the guard's unit file is
// deleted: FileDescriptorStorePreserve=yes keeps the store through a stop, and systemd 255.4 (Ubuntu 24.04) aborts
// PID 1 in the next daemon-reload when a unit with stored descriptors has no unit file (systemd issue #35288,
// fixed by #35292 in v257; 2026-11-19 the account probe's pair host died this way). clean refuses a running unit
// and reports an empty store or an unknown unit as an error, so the result is ignored.
func releaseGuardFDStore(ctx context.Context, run func(context.Context, string, ...string) ([]byte, error), service string) {
	_, _ = run(ctx, "systemctl", "clean", "--what=fdstore", service)
}

// guardInstallCommands never disables the guard unit: that empties systemd's descriptor store, and with it the
// listeners the replaced guard hands to the next one (lease_handoff_linux.go).
var guardInstallCommands = [][]string{{"daemon-reload"}, {"enable", "paperboat-machineguard-deny.service", "paperboat-machineguard.service"}, {"start", "paperboat-machineguard-deny.service"}, {"restart", "paperboat-machineguard.service"}}

const guardUnitMarker = "# Managed by Paperboat machineguard v1\n"

func validateGuardUnit(path string) error {
	if info, err := os.Lstat(path); err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
			return errors.New("unsafe machine guard unit path")
		}
		previous, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.HasPrefix(previous, []byte(guardUnitMarker)) {
			return errors.New("refusing to overwrite foreign machine guard unit")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}
func writeGuardUnit(path, unit string) error {
	if err := validateGuardUnit(path); err != nil {
		return err
	}
	return atomicfile.Write(path, []byte(guardUnitMarker+unit), atomicfile.CurrentOwnerOptions(0644))
}

const guardDenyUnit = `[Unit]
Description=Paperboat machine address isolation
DefaultDependencies=no
After=local-fs.target systemd-modules-load.service
Before=systemd-user-sessions.service

[Service]
Type=oneshot
ExecStart=/usr/local/libexec/paperboat-machineguard/pb daemon machine-guard restore-deny
RemainAfterExit=yes
TimeoutStartSec=10

[Install]
RequiredBy=systemd-user-sessions.service
`

// Units returns this build's guard unit files by name. An update installs the
// candidate build's own units, so the guard never runs another build's render.
func Units() map[string]string {
	return map[string]string{
		"paperboat-machineguard-deny.service": guardDenyUnit,
		"paperboat-machineguard.service": `[Unit]
Description=Paperboat protected machine names
Requires=paperboat-machineguard-deny.service
After=paperboat-machineguard-deny.service systemd-resolved.service
Wants=systemd-resolved.service

[Service]
Type=notify
NotifyAccess=main
TimeoutStartSec=30
ExecStart=/usr/local/libexec/paperboat-machineguard/pb daemon machine-guard run
Restart=on-failure
RestartSec=1
TimeoutStopSec=10
UMask=0077
FileDescriptorStoreMax=4096
FileDescriptorStorePreserve=yes

[Install]
WantedBy=multi-user.target
`,
	}
}
