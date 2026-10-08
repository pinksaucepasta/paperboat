//go:build linux

package machineguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Install copies the invoked executable into root-only installation storage and
// restores only address isolation before user sessions. DNS startup does not gate
// normal logins. No user-writable binary runs as root.
func Install(ctx context.Context, executable string) (resultErr error) {
	if err := requirePrivilege(); err != nil {
		return err
	}
	lifecycle, err := lockGuardLifecycle(DefaultStateDir)
	if err != nil {
		return err
	}
	defer lifecycle.Close()
	oldState, err := loadLoopbackState(DefaultStateDir)
	if err != nil {
		return err
	}
	if _, migrateErr := migrateLoopbackState(ctx, DefaultStateDir); migrateErr != nil {
		return migrateErr
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, writeLoopbackState(DefaultStateDir, oldState))
		}
	}()
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
	if err := ensureLocalTrustTools(ctx); err != nil {
		return err
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
	defer finishGuardInstallation(ctx, rollback, &resultErr, func(c context.Context) error {
		if !candidateStarted {
			return nil
		}
		return exec.CommandContext(c, "systemctl", "stop", "paperboat-machineguard.service").Run()
	}, func(c context.Context) error {
		if err := writeLoopbackState(DefaultStateDir, oldState); err != nil {
			return err
		}
		if err := exec.CommandContext(c, "systemctl", "daemon-reload").Run(); err != nil {
			return err
		}
		if wasRunning {
			return exec.CommandContext(c, "systemctl", "start", "paperboat-machineguard.service").Run()
		}
		return nil
	})
	if _, err := os.Lstat("/etc/systemd/system/paperboat-machineguard.service"); err == nil {
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
		return err
	}
	source, err := os.Open(executable)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.CreateTemp(directory, ".pb-")
	if err != nil {
		return err
	}
	temporary := target.Name()
	defer os.Remove(temporary)
	if _, err = io.Copy(target, source); err == nil {
		err = target.Chmod(0755)
	}
	if err == nil {
		err = target.Sync()
	}
	err = errors.Join(err, target.Close())
	if err != nil {
		return err
	}
	err = os.Rename(temporary, filepath.Join(directory, "pb"))
	rollback.record(filepath.Join(directory, "pb"))
	if err != nil {
		return err
	}
	units := map[string]string{
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

[Install]
WantedBy=multi-user.target
`,
	}
	for name, unit := range units {
		path := filepath.Join("/etc/systemd/system", name)
		err = writeGuardUnit(path, unit)
		rollback.record(path)
		if err != nil {
			return err
		}
	}
	for _, args := range [][]string{{"daemon-reload"}, {"disable", "paperboat-machineguard.service"}, {"enable", "paperboat-machineguard-deny.service", "paperboat-machineguard.service"}, {"start", "paperboat-machineguard-deny.service"}, {"restart", "paperboat-machineguard.service"}} {
		if args[0] == "restart" {
			candidateStarted = true
		}
		if output, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("machine guard service: %w: %s", err, output)
		}
	}
	return nil
}

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
	file, err := os.CreateTemp(filepath.Dir(path), ".paperboat-unit-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.WriteString(guardUnitMarker + unit); err == nil {
		err = file.Chmod(0644)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
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
