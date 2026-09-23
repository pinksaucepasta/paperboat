//go:build linux

package deviceguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Install copies the invoked executable into root-only installation storage and
// restores only address isolation before user sessions. DNS startup does not gate
// normal logins. No user-writable binary runs as root.
func Install(ctx context.Context, executable string, configuredCIDR ...string) (resultErr error) {
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
	cidr := oldState.Active
	if len(configuredCIDR) > 0 && strings.TrimSpace(configuredCIDR[0]) != "" {
		cidr = configuredCIDR[0]
	}
	cidr, fence, err := prepareLoopbackCIDRChange(ctx, cidr)
	if err != nil {
		return err
	}
	if fence != nil {
		defer fence.Close()
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, writeLoopbackState(DefaultStateDir, oldState))
		}
	}()
	if err := storeLoopbackCIDR(DefaultStateDir, cidr); err != nil {
		return err
	}

	if os.Geteuid() != 0 {
		return errors.New("installing the device guard requires root")
	}
	for _, name := range []string{"paperboat-deviceguard-deny.service", "paperboat-deviceguard.service"} {
		if err := validateGuardUnit(filepath.Join("/etc/systemd/system", name)); err != nil {
			return err
		}
	}
	directory := "/usr/local/libexec/paperboat-deviceguard"
	if err := protectedDirectory(directory, 0755); err != nil {
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
	if err = os.Rename(temporary, filepath.Join(directory, "pb")); err != nil {
		return err
	}
	units := map[string]string{
		"paperboat-deviceguard-deny.service": guardDenyUnit,
		"paperboat-deviceguard.service": `[Unit]
Description=Paperboat protected device names
Requires=paperboat-deviceguard-deny.service
After=paperboat-deviceguard-deny.service systemd-resolved.service
Wants=systemd-resolved.service

[Service]
Type=notify
NotifyAccess=main
TimeoutStartSec=30
ExecStart=/usr/local/libexec/paperboat-deviceguard/pb daemon device-guard run
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
		if err = writeGuardUnit(path, unit); err != nil {
			return err
		}
	}
	for _, args := range [][]string{{"daemon-reload"}, {"disable", "paperboat-deviceguard.service"}, {"enable", "paperboat-deviceguard-deny.service", "paperboat-deviceguard.service"}, {"start", "paperboat-deviceguard-deny.service"}, {"restart", "paperboat-deviceguard.service"}} {
		if output, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("device guard service: %w: %s", err, output)
		}
	}
	return nil
}

const guardUnitMarker = "# Managed by Paperboat deviceguard v1\n"

func validateGuardUnit(path string) error {
	if info, err := os.Lstat(path); err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
			return errors.New("unsafe device guard unit path")
		}
		previous, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.HasPrefix(previous, []byte(guardUnitMarker)) {
			return errors.New("refusing to overwrite foreign device guard unit")
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
Description=Paperboat device address isolation
DefaultDependencies=no
After=local-fs.target systemd-modules-load.service
Before=systemd-user-sessions.service

[Service]
Type=oneshot
ExecStart=/usr/local/libexec/paperboat-deviceguard/pb daemon device-guard restore-deny
RemainAfterExit=yes
TimeoutStartSec=10

[Install]
RequiredBy=systemd-user-sessions.service
`
