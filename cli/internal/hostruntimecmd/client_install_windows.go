//go:build windows

package hostruntimecmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"

	clientconfig "github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	helperconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
)

type ClientInstallConfig struct {
	StateRoot, WorkspaceRoot, ControlURL, MachineID, ListenAddress string
	Artifact                                                       bootstrap.ArtifactTarget
}

// InstallRunningBinary stages caller-supplied bytes into Windows' fixed,
// per-user machine-wide slot. Account enrollment remains a separate action.
func InstallRunningBinary(ctx context.Context, sourcePath string, source installsource.Source, installDir string) (string, error) {
	if ctx == nil || !filepath.IsAbs(sourcePath) || filepath.Clean(sourcePath) != sourcePath || installDir != "" {
		return "", errors.New("invalid Windows installation source")
	}
	if err := source.Validate(); err != nil || source.Platform != "windows" || source.Architecture != runtime.GOARCH {
		return "", errors.New("invalid Windows installation source identity")
	}
	if err := source.Verify(sourcePath); err != nil {
		return "", err
	}
	account, err := user.Current()
	if err != nil || account.Username == "" {
		return "", errors.New("could not resolve Windows installation owner")
	}
	sid, err := currentBootstrapSID()
	if err != nil {
		return "", err
	}
	instance, err := hostinstall.WindowsInstanceForSID(sid)
	if err != nil {
		return "", err
	}
	layout, err := hostinstall.WindowsLayoutForInstance(instance)
	if err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	stateRoot, err := helperconfig.DefaultStateRoot(os.Getenv)
	if err != nil {
		return "", err
	}
	client, err := clientconfig.Load("")
	if err != nil {
		return "", err
	}
	request := hostinstall.Request{Schema: hostinstall.SchemaV1, Platform: "windows", User: windowsAccountName(account.Username), Group: "Paperboat", OwnerSID: sid, Executable: sourcePath, Source: source, StateRoot: stateRoot, WorkspaceRoot: home, ControlURL: client.ServerURL, SetupMode: "awaiting_enrollment"}
	action := elevation.ActionInstall
	if existing, loadErr := hostinstall.LoadWindowsRuntimeConfigForInstance(instance); loadErr == nil {
		if existing.SetupMode != "awaiting_enrollment" {
			request.Artifact = existing.Artifact
			request.Home = home
			request.Path = os.Getenv("PATH")
			request.StateRoot = existing.StateRoot
			request.WorkspaceRoot = existing.Workspace
			request.ControlURL = existing.ControlURL
			request.UserMachineID = existing.MachineID
			request.Shell = filepath.Join(os.Getenv("WINDIR"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
			request.HelperListenAddress = existing.ListenAddress
			request.SetupMode = existing.SetupMode
			action = elevation.ActionInstallCommit
		}
	} else if !errors.Is(loadErr, os.ErrNotExist) && !errors.Is(loadErr, hostinstall.ErrNotInstalled) {
		return "", loadErr
	}
	elevatedPath, cleanup, err := stageWindowsElevationExecutable(sourcePath)
	if err != nil {
		return "", err
	}
	defer cleanup()
	if err := elevation.RunRuntimeService(ctx, elevatedPath, action, request); err != nil {
		return "", err
	}
	return layout.Binary, nil
}

func stageWindowsElevationExecutable(sourcePath string) (string, func(), error) {
	temporary, err := os.CreateTemp("", ".pb-elevated-*.exe")
	if err != nil {
		return "", func() {}, err
	}
	path := temporary.Name()
	cleanup := func() { _ = os.Remove(path) }
	source, err := os.Open(sourcePath)
	if err != nil {
		temporary.Close()
		cleanup()
		return "", func() {}, err
	}
	_, copyErr := io.Copy(temporary, source)
	err = errors.Join(copyErr, source.Close(), temporary.Close())
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	return path, cleanup, nil
}

// InstallClient shares the same verified artifact, slots, service contract,
// and rollback behavior as bootstrap. No Client-only Windows service exists.
func InstallClient(ctx context.Context, config ClientInstallConfig, _ io.Reader, stdout, _ io.Writer) error {
	if !filepath.IsAbs(config.StateRoot) || !filepath.IsAbs(config.WorkspaceRoot) || config.MachineID == "" {
		return errors.New("invalid Windows Client installation")
	}
	artifactPath, sourceIdentity, err := installsource.Current()
	if err != nil {
		return err
	}
	account, err := user.Current()
	if err != nil {
		return err
	}
	sid, err := currentBootstrapSID()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	request := hostinstall.Request{Schema: hostinstall.SchemaV1, Platform: runtime.GOOS, User: windowsAccountName(account.Username), Group: "Paperboat", OwnerSID: sid, Executable: artifactPath, Artifact: config.Artifact, Source: sourceIdentity, Home: home, Path: os.Getenv("PATH"), StateRoot: config.StateRoot, WorkspaceRoot: config.WorkspaceRoot, ControlURL: config.ControlURL, UserMachineID: config.MachineID, Shell: filepath.Join(os.Getenv("WINDIR"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"), HelperListenAddress: config.ListenAddress, SetupMode: "client"}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err := elevation.RunRuntimeService(ctx, executable, elevation.ActionInstallCommit, request); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Paperboat Windows device service is ready.")
	return nil
}
