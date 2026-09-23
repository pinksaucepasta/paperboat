//go:build darwin || linux

package hostruntimecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	documentation "github.com/pinksaucepasta/paperboat/docs"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

func InstallRunningBinary(ctx context.Context, executable string, source installsource.Source, directory string) (string, error) {
	if ctx == nil {
		return "", hostinstall.ErrInvalidRequest
	}
	if err := source.Verify(executable); err != nil {
		return "", err
	}
	account, err := user.Current()
	if err != nil {
		return "", err
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return "", err
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return "", err
	}
	group, err := user.LookupGroupId(account.Gid)
	if err != nil {
		return "", err
	}
	if directory == "" {
		directory = filepath.Join(account.HomeDir, ".local", "bin")
	}
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return "", errors.New("install directory must be an absolute canonical path")
	}
	layout, err := service.UserLayout(runtime.GOOS, uid)
	if err != nil {
		return "", err
	}
	paths, err := localdaemon.CurrentUserPaths()
	if err != nil {
		return "", err
	}
	lock, err := hostinstall.LockSuppliedOperation(filepath.Join(paths.StateRoot, "install.lock"), uid)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	request := hostinstall.Request{Schema: hostinstall.SchemaV1, Platform: runtime.GOOS, User: account.Username, UID: uid, GID: gid, Group: group.Name, Home: account.HomeDir, Executable: executable, Source: source, SetupMode: "awaiting_enrollment"}
	invoke := func(callCtx context.Context, operation string) error {
		body, err := json.Marshal(request)
		if err != nil {
			return err
		}
		args := []string{"--", "/usr/bin/env", "PAPERBOAT_INVOKING_UID=" + account.Uid, executable, "__runtime-service", operation}
		command := exec.CommandContext(callCtx, "/usr/bin/sudo", args...)
		if os.Geteuid() == 0 {
			command = exec.CommandContext(callCtx, "/usr/bin/env", args[2:]...)
		}
		command.Stdin = bytes.NewReader(body)
		command.Stdout, command.Stderr = os.Stderr, os.Stderr
		if err = command.Run(); err != nil {
			return fmt.Errorf("install supplied Paperboat executable: %w", err)
		}
		return nil
	}
	if err = invoke(ctx, "install-supplied"); err != nil {
		return "", err
	}
	link, err := installWorkerCommand(directory, layout.Binary)
	var restoreService func(context.Context) error
	rollback := func(cause error) (string, error) {
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
		defer cancel()
		var linkErr error
		if link != nil {
			linkErr = link.Rollback()
		}
		binaryErr := invoke(recovery, "rollback-supplied")
		var serviceErr error
		if restoreService != nil {
			serviceErr = restoreService(recovery)
		}
		return "", errors.Join(cause, binaryErr, serviceErr, linkErr)
	}
	if err != nil {
		return rollback(err)
	}
	cfg, err := config.Load("")
	if err != nil {
		return rollback(err)
	}
	if restoreService, err = localdaemon.ReplaceCurrentUserService(ctx, layout.Binary, cfg.Path(), cfg.ServerURL); err != nil {
		return rollback(err)
	}
	client, err := localapi.NewClient(paths.SocketPath, time.Second)
	if err != nil {
		return rollback(err)
	}
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		snapshot, probeErr := client.Snapshot(readyCtx)
		if probeErr == nil && snapshot.DaemonVersion == source.Version && snapshot.DaemonState != "starting" && snapshot.DaemonState != "stopping" {
			break
		}
		select {
		case <-readyCtx.Done():
			return rollback(errors.New("installed Paperboat service did not start; attempting to restore the previous installation"))
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err = invoke(ctx, "commit-supplied"); err != nil {
		return rollback(err)
	}
	if err = link.Commit(); err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		manuals := exec.CommandContext(ctx, "/usr/bin/sudo", "--", layout.Binary, "--no-customization", "__man-pages", "--directory", "/usr/local/share/man")
		manuals.Stdin, manuals.Stdout, manuals.Stderr = os.Stdin, os.Stderr, os.Stderr
		err = manuals.Run()
	} else {
		err = documentation.Install(filepath.Join(filepath.Dir(directory), "share", "man"))
	}
	if err != nil {
		return "", fmt.Errorf("Paperboat is installed, but its manuals could not be installed; retry pb install to repair them: %w", err)
	}
	return filepath.Join(directory, "pb"), nil
}
