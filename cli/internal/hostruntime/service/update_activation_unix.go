//go:build darwin || linux

package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"howett.net/plist"
)

const UnixUpdateActivatorUnit = "paperboat-update-activate.service"
const UnixUpdateActivatorLabel = "com.pinksaucepasta.paperboat.update-activate"

// UnixUpdateActivator controls fixed native jobs for the enrolled installation.
// Hostd restart is invoked only after the update transaction fences terminal
// admission and installs a verified executable in the canonical slot.
type UnixUpdateActivator struct {
	Platform string
	UID      int
	Runner   Runner
}

func (a UnixUpdateActivator) definition() string {
	if a.Platform == "darwin" {
		return "/Library/LaunchDaemons/" + a.activatorLabel() + ".plist"
	}
	return "/etc/systemd/system/" + a.activatorUnit()
}
func (a UnixUpdateActivator) instance() string { return "u" + strconv.Itoa(a.UID) }
func (a UnixUpdateActivator) activatorUnit() string {
	return strings.TrimSuffix(UnixUpdateActivatorUnit, ".service") + "-" + a.instance() + ".service"
}
func (a UnixUpdateActivator) activatorLabel() string {
	return UnixUpdateActivatorLabel + "." + a.instance()
}

func (a UnixUpdateActivator) Install(ctx context.Context, binary string, environment map[string]string) error {
	if a.Runner == nil || a.Platform != runtime.GOOS || a.UID < 0 || os.Geteuid() != 0 {
		return ErrInvalidDefinition
	}
	if err := safeExecutableForInstall(binary, false); err != nil {
		return err
	}
	if environment["PAPERBOAT_UPDATE_STATE_ROOT"] == "" || binary != filepath.Join(environment["PAPERBOAT_UPDATE_STATE_ROOT"], "activation", "pb") {
		return ErrInvalidDefinition
	}
	data, err := unixUpdateActivatorDefinition(a.Platform, a.UID, binary, environment)
	if err != nil {
		return err
	}
	path := a.definition()
	if info, err := os.Lstat(path); err == nil {
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
			return ErrInvalidDefinition
		}
		current, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if string(current) != string(data) {
			return errors.New("existing update activation job belongs to another installation")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = atomicfile.Write(path, data, atomicfile.Options{Mode: 0644, OwnerUID: 0, OwnerGID: 0}); err != nil {
		return err
	}
	if a.Platform == "linux" {
		if err = a.Runner.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			return err
		}
		if err = a.Runner.Run(ctx, "systemctl", "enable", a.activatorUnit()); err != nil {
			return err
		}
		return a.Runner.Run(ctx, "systemctl", "start", "--no-block", a.activatorUnit())
	}
	if err = a.Runner.Run(ctx, "launchctl", "enable", "system/"+a.activatorLabel()); err != nil {
		return err
	}
	controller := LaunchdController{Runner: a.Runner, UID: a.UID, Label: a.activatorLabel()}
	status, err := controller.Inspect(ctx, path)
	if err != nil {
		return err
	}
	if status.Running {
		return nil
	}
	if status.Registered {
		return a.Runner.Run(ctx, "launchctl", "kickstart", "system/"+a.activatorLabel())
	}
	return a.Runner.Run(ctx, "launchctl", "bootstrap", "system", path)
}

// Retire disables reboot/restart activation and removes its declaration, but
// lets the calling helper finish durable cleanup and exit successfully. It must
// not kill itself between retiring the job and removing the handoff marker.
func (a UnixUpdateActivator) Retire(ctx context.Context) error {
	if a.Runner == nil || a.Platform != runtime.GOOS || os.Geteuid() != 0 {
		return ErrInvalidDefinition
	}
	info, err := os.Lstat(a.definition())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return ErrInvalidDefinition
	}
	if a.Platform == "linux" {
		if err := a.Runner.Run(ctx, "systemctl", "disable", a.activatorUnit()); err != nil {
			return err
		}
	}
	if a.Platform == "darwin" {
		if err := a.Runner.Run(ctx, "launchctl", "disable", "system/"+a.activatorLabel()); err != nil {
			return err
		}
	}
	if err := os.Remove(a.definition()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if a.Platform == "linux" {
		if err := a.Runner.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			return err
		}
	}
	directory, err := os.Open(filepath.Dir(a.definition()))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
func (a UnixUpdateActivator) RestartUpdater(ctx context.Context) error {
	if a.Runner == nil || a.Platform != runtime.GOOS || a.UID < 0 {
		return ErrInvalidDefinition
	}
	if a.Platform == "linux" {
		return a.Runner.Run(ctx, "systemctl", "restart", "paperboat-updated-"+a.instance()+".service")
	}
	return a.Runner.Run(ctx, "launchctl", "kickstart", "-k", "system/"+UpdaterLabel+"."+a.instance())
}

// RestartHostd actuates the canonical runtime after verified slot rotation.
// The transaction owns admission fencing, readiness, and policy-valid rollback.
func (a UnixUpdateActivator) RestartHostd(ctx context.Context) error {
	if ctx == nil || a.Runner == nil || a.Platform != runtime.GOOS || a.UID < 0 {
		return ErrInvalidDefinition
	}
	if a.Platform == "linux" {
		return a.Runner.Run(ctx, "systemctl", "restart", "paperboat-hostd-"+a.instance()+".service")
	}
	if a.Platform == "darwin" {
		return a.Runner.Run(ctx, "launchctl", "kickstart", "-k", "system/"+HostdLabel+"."+a.instance())
	}
	return ErrUnsupportedPlatform
}

func unixUpdateActivatorDefinition(platform string, uid int, binary string, environment map[string]string) ([]byte, error) {
	if !filepath.IsAbs(binary) || filepath.Clean(binary) != binary || strings.ContainsAny(binary, "\x00\r\n") {
		return nil, ErrInvalidDefinition
	}
	allowed := map[string]bool{}
	for _, key := range []string{"PAPERBOAT_UPDATE_STATE_ROOT", "PAPERBOAT_BINARY", "PAPERBOAT_BINARY_ROLLBACK", "PAPERBOAT_BINARY_STAGED", "PAPERBOAT_RELEASE_ROOT", "PAPERBOAT_HOSTD_SOCKET", "PAPERBOAT_HOSTD_TOKEN_FILE", "PAPERBOAT_RELEASE_REPOSITORY", "PAPERBOAT_MACHINE_ID", "PAPERBOAT_UPDATE_HEALTH_URL", "PAPERBOAT_ENROLLED_UID", "PAPERBOAT_ENROLLED_GID", "PAPERBOAT_UPDATED_SOCKET"} {
		allowed[key] = true
	}
	values := map[string]string{}
	for key, value := range environment {
		if !allowed[key] || strings.ContainsAny(value, "\x00\r\n") {
			return nil, ErrInvalidDefinition
		}
		values[key] = value
	}
	arguments := []string{binary, "daemon", "__runtime-updated", "--activation-helper"}
	if platform == "darwin" {
		return plist.Marshal(map[string]any{"Label": UnixUpdateActivatorLabel + ".u" + strconv.Itoa(uid), "ProgramArguments": arguments, "EnvironmentVariables": values, "UserName": "root", "RunAtLoad": true, "KeepAlive": map[string]bool{"SuccessfulExit": false}, "ThrottleInterval": 5}, plist.XMLFormat)
	}
	if platform != "linux" {
		return nil, ErrUnsupportedPlatform
	}
	var body strings.Builder
	body.WriteString("[Unit]\nDescription=Paperboat verified update activation\nAfter=paperboat-hostd-u" + strconv.Itoa(uid) + ".service\n[Service]\nType=simple\nExecStart=")
	for i, value := range arguments {
		if i > 0 {
			body.WriteByte(' ')
		}
		body.WriteString(systemdEscape(value))
	}
	body.WriteByte('\n')
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		body.WriteString("Environment=" + systemdEscape(key+"="+values[key]) + "\n")
	}
	body.WriteString("Restart=on-failure\nRestartSec=5\n[Install]\nWantedBy=multi-user.target\n")
	return []byte(body.String()), nil
}

// RemoveForUninstall waits for the fixed helper to stop while the caller holds
// activation ownership. The helper calls Retire without stopping itself.
func (a UnixUpdateActivator) RemoveForUninstall(ctx context.Context) error {
	if err := a.Retire(ctx); err != nil {
		return err
	}
	if a.Platform == "linux" {
		return (SystemdController{Runner: a.Runner, Unit: a.activatorUnit()}).Stop(ctx, a.definition())
	}
	return (LaunchdController{Runner: a.Runner, UID: a.UID, Label: a.activatorLabel()}).Stop(ctx, a.definition())
}
