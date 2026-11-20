package main

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/command"
	"github.com/pinksaucepasta/paperboat/cli/internal/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/cli/internal/localdaemon"
	"github.com/pinksaucepasta/paperboat/cli/internal/machineguard"
	"github.com/pinksaucepasta/paperboat/cli/internal/resolver"
	"github.com/spf13/cobra"
)

// installDependencies are the edges of install, setup, fresh enrollment, local access and the commands that start
// the local daemon or move files: what registers a service, installs the running binary, prepares the host,
// removes or replaces a machine record, asks the machine guard, and opens a transfer.
type installDependencies struct {
	// installService owns the one production boundary that registers the local daemon.
	installService     localDaemonServiceInstaller
	installExecutable  func(context.Context, string, installsource.Source, string, string) (string, error)
	runningSource      func() (string, installsource.Source, error)
	verifySetupSource  func() error
	setupBackend       func(*command.Context) (*api.Client, error)
	setupPrerequisites func(context.Context) (uint16, error) // Windows: OpenSSH, with an administrator prompt
	freshCleanup       func(*cobra.Command) error
	rememberReplaced   func() (string, error)
	replaceMachine     func(*command.Context, string, string) error
	freshExecutable    func() (string, error)
	transferClient     func(context.Context, *resolver.FileTransferTarget, api.UserMachine, string) (*filetransfer.NativeClient, io.Closer, error)
	guardStatus        func(context.Context) (machineguard.RuntimeStatus, error) // the only owner of the local access domain
	applyLocalAccess   func(context.Context, *config.Config, string) error
	disconnectMachine  func(*command.Context) error
}

func defaultInstallDependencies() installDependencies {
	return installDependencies{
		installService:     installCurrentUserService,
		installExecutable:  hostruntimecmd.InstallRunningBinary,
		runningSource:      installsource.Current,
		verifySetupSource:  hostruntimecmd.VerifyInstallSource,
		setupBackend:       backendClient,
		setupPrerequisites: setupPlatformHostPrerequisites,
		freshCleanup:       runFreshEnrollmentCleanup,
		rememberReplaced:   rememberReplacedMachine,
		replaceMachine:     replaceUserMachine,
		freshExecutable:    existingFreshEnrollmentExecutable,
		transferClient:     openTransferCommandClient,
		guardStatus:        machineGuardStatus,
		applyLocalAccess:   applyLocalAccessConfig,
		disconnectMachine:  disconnectOwnMachine,
	}
}

func installDependenciesOf(ctx context.Context) installDependencies {
	return dependenciesIn(ctx, defaultInstallDependencies)
}

// installCurrentUserService registers the local daemon with the platform. A Go test executable must never be
// registered as a detached daemon service (it can start recursive test processes that keep files and named pipes
// after the test exits), so a test binary refuses; command tests inject their own installer where they need one.
func installCurrentUserService(ctx context.Context, executable, configPath, server string) error {
	if testing.Testing() {
		return errors.New("local daemon installation is disabled during command tests")
	}
	return localdaemon.InstallCurrentUserService(ctx, executable, configPath, server)
}
