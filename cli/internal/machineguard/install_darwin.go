//go:build darwin

package machineguard

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

const darwinHelperPath = "/Library/PrivilegedHelperTools/io.paperboat.machineguard"
const darwinLaunchPath = "/Library/LaunchDaemons/io.paperboat.machineguard.plist"
const darwinLaunchMarker = "<!-- Managed by Paperboat machine guard -->"

func Install(ctx context.Context, executable string) (resultErr error) {
	if err := requirePrivilege(); err != nil {
		return err
	}
	// The control socket is reachable by user processes, while guard state
	// remains private. Create the shared application parent before the private
	// state directory can create it with mode 0700.
	parent := filepath.Dir(DefaultStateDir)
	if err := protectedDirectory(parent, 0755); err != nil {
		return err
	}
	if err := os.Chmod(parent, 0755); err != nil {
		return err
	}
	controlDirectory := filepath.Dir(DefaultSocket)
	if err := protectedDirectory(controlDirectory, 0755); err != nil {
		return err
	}
	// Launchd uses umask 077. Explicitly set the protected control directory's
	// traversal mode so authenticated non-root clients can reach its socket.
	if err := os.Chmod(controlDirectory, 0755); err != nil {
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

	return installDarwin(ctx, executable, darwinInstallConfig{
		HelperPath: darwinHelperPath, LaunchPath: darwinLaunchPath,
		Label: "io.paperboat.machineguard", Socket: DefaultSocket, Account: darwinBrokerAccount, StateDir: DefaultStateDir,
		LaunchArguments: []string{"daemon", "machine-guard", "run"}, InstallAccount: true, RollbackState: &oldState,
	})
}

type darwinInstallConfig struct {
	HelperPath, LaunchPath, Label, Socket, Account string
	StateDir                                       string
	RollbackState                                  *loopbackState
	LaunchArguments                                []string
	InstallAccount                                 bool
}

func installDarwin(ctx context.Context, executable string, cfg darwinInstallConfig) (resultErr error) {
	if err := requirePrivilege(); err != nil {
		return err
	}
	if cfg.HelperPath == "" || cfg.LaunchPath == "" || cfg.Label == "" || cfg.Socket == "" || !filepath.IsAbs(cfg.HelperPath) || !filepath.IsAbs(cfg.LaunchPath) || !filepath.IsAbs(cfg.Socket) || strings.ContainsAny(cfg.Label, "<>&/\x00\r\n") {
		return errors.New("invalid Darwin machine guard install configuration")
	}
	for _, directory := range []string{filepath.Dir(cfg.HelperPath), filepath.Dir(cfg.LaunchPath)} {
		if err := protectedDirectory(directory, 0755); err != nil {
			return err
		}
	}
	if err := darwinCheckInstallOwnership(cfg); err != nil {
		return err
	}
	if cfg.InstallAccount {
		if err := darwinInstallAccount(ctx, cfg.Account); err != nil {
			return err
		}
	}
	if old, err := os.ReadFile(cfg.LaunchPath); err == nil && !strings.Contains(string(old), darwinLaunchMarker) {
		return errors.New("existing machine guard launch service is not owned by Paperboat")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	source, err := os.Open(executable)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.CreateTemp(filepath.Dir(cfg.HelperPath), ".paperboat-helper-")
	if err != nil {
		return err
	}
	temp := target.Name()
	defer os.Remove(temp)
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
	rollback, err := snapshotGuardFiles(cfg.HelperPath, cfg.LaunchPath)
	if err != nil {
		return err
	}
	controller := service.LaunchdController{Runner: service.ExecRunner{}, UID: 0, Label: cfg.Label}
	wasRunning := exec.CommandContext(ctx, "/bin/launchctl", "print", "system/"+cfg.Label).Run() == nil
	defer finishGuardInstallation(ctx, rollback, &resultErr, func(c context.Context) error { return controller.Stop(c, cfg.LaunchPath) }, func(c context.Context) error {
		if cfg.RollbackState != nil {
			if err := writeLoopbackState(cfg.StateDir, *cfg.RollbackState); err != nil {
				return err
			}
		}
		if wasRunning {
			return controller.Start(c, cfg.LaunchPath)
		}
		return nil
	})
	if cfg.StateDir != "" {
		if err := controller.Stop(ctx, cfg.LaunchPath); err != nil {
			return err
		}

		if err := PrepareLocalCARenewal(ctx, Config{StateDir: cfg.StateDir}); err != nil {
			return fmt.Errorf("renew local browser trust: %w", err)
		}
		if err := prepareInstalledLocalCA(ctx, Config{StateDir: cfg.StateDir}); err != nil {
			return err
		}
	}
	programArguments := append([]string{cfg.HelperPath}, cfg.LaunchArguments...)
	var argumentXML strings.Builder
	for _, argument := range programArguments {
		argumentXML.WriteString("<string>")
		if err := xml.EscapeText(&argumentXML, []byte(argument)); err != nil {
			return err
		}
		argumentXML.WriteString("</string>")
	}
	unit := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
%s
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array>%s</array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
<key>ProcessType</key><string>Background</string><key>ThrottleInterval</key><integer>2</integer>
<key>Umask</key><integer>63</integer>
</dict></plist>
`, darwinLaunchMarker, cfg.Label, argumentXML.String())
	err = darwinWrite(cfg.LaunchPath, []byte(unit), 0644)
	rollback.record(cfg.LaunchPath)
	if err != nil {
		return err
	}
	if _, err = darwinRun(ctx, "", "/usr/bin/plutil", "-lint", cfg.LaunchPath); err != nil {
		return err
	}
	err = os.Rename(temp, cfg.HelperPath)
	rollback.record(cfg.HelperPath)
	if err != nil {
		return err
	}
	// A prior installed copy can be running; only this exact service is replaced.
	domain := "system/" + cfg.Label

	if err := controller.Apply(ctx, cfg.LaunchPath, true); err != nil {
		return err
	}
	ready, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		client, connectErr := Connect(ready, cfg.Socket)
		if connectErr == nil {
			connectErr = client.ReplaceNames(ready, nil)
			client.Close()
			if connectErr == nil {
				return nil
			}
		}
		select {
		case <-ready.Done():
			return fmt.Errorf("machine guard installed but not ready; inspect launchctl print %s and retry installation: %w", domain, ready.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func darwinCheckInstallOwnership(cfg darwinInstallConfig) error {
	ownedUnit := false
	for _, path := range []string{cfg.LaunchPath, cfg.HelperPath} {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("existing guard installation path is not a protected root-owned file: %s", path)
		}
		if path == cfg.LaunchPath {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			ownedUnit = strings.Contains(string(data), darwinLaunchMarker)
			if !ownedUnit {
				return errors.New("existing guard launch service is not owned by Paperboat")
			}
		} else if !ownedUnit {
			return errors.New("existing guard executable has no Paperboat service ownership record")
		}
	}
	return nil
}

func darwinInstallAccount(ctx context.Context, account string) error {
	if account == "" || strings.ContainsAny(account, "/:\x00\r\n") {
		return errors.New("invalid machine guard account")
	}
	path := "/Users/" + account
	listing, err := exec.CommandContext(ctx, "/usr/bin/dscl", ".", "-list", "/Users").Output()
	if err != nil {
		return err
	}
	exists := false
	for _, name := range strings.Fields(string(listing)) {
		if name == account {
			exists = true
		}
	}
	if exists {
		marker, err := exec.CommandContext(ctx, "/usr/bin/dscl", ".", "-read", path, "RealName").Output()
		if err != nil || !strings.Contains(string(marker), "Paperboat protected listener") {
			return errors.New("existing guard account is not owned by Paperboat")
		}
		// Read the complete owned record: a missing UniqueID after interruption is
		// different from an unreadable record or an existing invalid identity.
		record, readErr := exec.CommandContext(ctx, "/usr/bin/dscl", ".", "-read", path).Output()
		if readErr != nil {
			return readErr
		}
		foundUID := false
		for _, line := range strings.Split(string(record), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 || fields[0] != "UniqueID:" {
				continue
			}
			foundUID = true
			if len(fields) != 2 {
				return errors.New("existing guard account has an invalid system UID")
			}
			uid, parseErr := strconv.Atoi(fields[1])
			if parseErr != nil || uid < 400 || uid > 499 {
				return errors.New("existing guard account has an invalid system UID")
			}
		}
		if foundUID {
			// Never reassign an existing identity. Complete its disabled login state.
			for _, fields := range [][]string{{"PrimaryGroupID", "20"}, {"UserShell", "/usr/bin/false"}, {"NFSHomeDirectory", "/var/empty"}, {"IsHidden", "1"}, {"Password", "*"}, {"AuthenticationAuthority", ";DisabledUser;"}} {
				if _, err = darwinRun(ctx, "", "/usr/bin/dscl", append([]string{".", "-create", path}, fields...)...); err != nil {
					return err
				}
			}
			return nil
		}
		// Our marker exists but no identity was assigned: finish the interrupted
		// creation using a currently unused UID, written only after disabling login.

	}
	output, err := exec.CommandContext(ctx, "/usr/bin/dscl", ".", "-list", "/Users", "UniqueID").Output()
	if err != nil {
		return err
	}
	used := map[int]bool{}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			if id, e := strconv.Atoi(fields[1]); e == nil {
				used[id] = true
			}
		}
	}
	uid := 499
	for uid >= 400 && used[uid] {
		uid--
	}
	if uid < 400 {
		return errors.New("no unused system UID for the machine guard")
	}
	// A non-login service identity owns only listener sockets. It has no account
	// credentials, writable application state, or interactive shell.
	for _, fields := range [][]string{{"RealName", "Paperboat protected listener"}, {"PrimaryGroupID", "20"}, {"UserShell", "/usr/bin/false"}, {"NFSHomeDirectory", "/var/empty"}, {"IsHidden", "1"}, {"Password", "*"}, {"AuthenticationAuthority", ";DisabledUser;"}, {"UniqueID", strconv.Itoa(uid)}} {
		args := append([]string{".", "-create", path}, fields...)
		if _, err = darwinRun(ctx, "", "/usr/bin/dscl", args...); err != nil {
			return err
		}
	}
	return nil
}
