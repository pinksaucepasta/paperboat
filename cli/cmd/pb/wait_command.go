package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
	"github.com/pinksaucepasta/paperboat/internal/localwait"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/spf13/cobra"
)

func waitCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "wait <machine>",
		Short: "Wait for a machine readiness condition",
		Args:  commandArgs(cobra.ExactArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			condition, _ := command.Flags().GetString("for")
			if condition != "runtime" && condition != "transport" && condition != "ssh" {
				return invocationError(errors.New("--for must be runtime, transport, or ssh"))
			}
			timeout, _ := command.Flags().GetDuration("timeout")
			if timeout <= 0 || timeout > 24*time.Hour {
				return invocationError(errors.New("--timeout must be greater than zero and no more than 24h"))
			}
			client, snapshot, err := localDaemonSnapshot(command, installLocalDaemonService)
			if err != nil {
				return fmt.Errorf("start local Paperboat daemon: %w", err)
			}
			waitCtx, cancel := context.WithTimeout(command.Context(), timeout)
			defer cancel()
			// Presence changes no longer wake every daemon of the account; while waiting, ask our own daemon.
			go localwait.KeepInventoryFresh(waitCtx, client.RefreshInventory, localwait.InventoryRefreshEvery, localwait.InventoryRefreshMaximum)
			var result localwait.Result
			if condition == "runtime" {
				result, err = localwait.WaitTargetFromSnapshot(waitCtx, client, snapshot, args[0], condition)
			} else {
				result, err = waitForAuthenticatedTransport(waitCtx, command, client, snapshot, args[0], condition)
			}
			if err != nil {
				return fmt.Errorf("wait for local Paperboat status: %w", err)
			}
			jsonOutput, _ := command.Flags().GetBool("json")
			if jsonOutput {
				encoder := json.NewEncoder(command.OutOrStdout())
				encoder.SetEscapeHTML(false)
				if err := encoder.Encode(result); err != nil {
					return err
				}
			} else {
				writeWaitResult(command.OutOrStdout(), command.ErrOrStderr(), result)
			}
			if code := waitExitCode(result); code != 0 {
				return exitCodeError{code: code}
			}
			return nil
		},
	}
	command.Flags().String("for", "transport", "readiness condition: runtime, transport, or ssh")
	command.Flags().Duration("timeout", 5*time.Minute, "maximum time to wait")
	command.Flags().Bool("json", false, "print JSON")
	return command
}

func waitForAuthenticatedTransport(ctx context.Context, command *cobra.Command, client *localapi.Client, snapshot localapi.Snapshot, target, condition string) (localwait.Result, error) {
	dependencies, err := buildDeps(actionContext(command, []string{target}))
	if err != nil {
		return localwait.Result{}, err
	}
	if dependencies.peerLocal == nil {
		return localwait.Result{}, errors.New("authenticated peer transport is unavailable")
	}
	backend, err := backendClient(actionContext(command, []string{target}))
	if err != nil {
		return localwait.Result{}, err
	}
	for {
		localMachine, resolveErr := localwait.ResolveMachine(snapshot.Machines, target)
		if resolveErr != nil {
			return localwait.Result{}, resolveErr
		}
		ready := localMachine.Eligible && localMachine.Generation > 0 && (localMachine.RuntimeState == "ready" || localMachine.RuntimeState == "degraded")
		if condition == "ssh" {
			ready = ready && localMachine.SSHReadiness == "ready"
		}
		if ready {
			machine, machineErr := resolveUserMachine(ctx, backend, localMachine.ID)
			if machineErr == nil && machine.Online {
				info := resolver.ConnectInfo{TargetKind: "machine", MachineID: machine.ID, Machine: machine.Alias, MachineGeneration: uint64(machine.InstallationGeneration), Terminal: &resolver.TerminalTarget{Protocol: "paperboat.health-probe.v1", EnvironmentID: machine.EnvironmentID}}
				probeCtx, cancelProbe := context.WithTimeout(ctx, 10*time.Second)
				probe, probeErr := probeDaemonPeer(probeCtx, client, info, "wait_transport")
				cancelProbe()
				if probeErr == nil {
					for index := range snapshot.Machines {
						if snapshot.Machines[index].ID == localMachine.ID {
							snapshot.Machines[index].SelectedPath = nativeSnapshotPath(probe.Path)
							snapshot.Machines[index].RelayRegion = ""
						}
					}
					return localwait.WaitTargetFromSnapshot(ctx, client, snapshot, localMachine.ID, condition)
				}
				if errors.Is(probeErr, localapi.ErrPermission) {
					return localwait.Result{}, probeErr
				}
			}
		}
		select {
		case <-ctx.Done():
			return localwait.WaitTargetFromSnapshot(ctx, client, snapshot, localMachine.ID, condition)
		case <-time.After(time.Second):
		}
		snapshot, err = client.Snapshot(ctx)
		if err != nil {
			return localwait.Result{}, err
		}
	}
}

type localDaemonServiceInstaller func(context.Context, string, string, string) error

type localDaemonServiceRemover func(context.Context, string) error

type localDaemonSnapshotClient interface {
	Snapshot(context.Context) (localapi.Snapshot, error)
}

type localDaemonSnapshotLauncher func(context.Context) error

const (
	localDaemonReadyTimeout      = 30 * time.Second
	localDaemonReadyPollInterval = 50 * time.Millisecond
)

var localDaemonLaunchGate = make(chan struct{}, 1)

// localDaemonRegistrationMatches reports whether this machine has a machine
// registration for the active server. A clean sign-in intentionally has no
// registration yet: auth must not launch a daemon that can only exit with a
// setup-required error.
func localDaemonRegistrationMatches(cfg *config.Config) (bool, error) {
	if cfg == nil || strings.TrimSpace(cfg.ServerURL) == "" {
		return false, localdaemon.ErrInvalidInventoryConfig
	}
	store, err := runtimeIdentityStore()
	if err != nil {
		return false, err
	}
	registration, err := store.Registration()
	if onlyNotExistCause(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	configuredServer, err := config.NormalizeServerURL(cfg.ServerURL)
	if err != nil {
		return false, err
	}
	registeredServer, err := config.NormalizeServerURL(registration.ServerURL)
	if err != nil {
		return false, err
	}
	return registration.MachineID != "" && registeredServer == configuredServer, nil
}

func localDaemonLaunchValues(cfg *config.Config) (string, string, string, error) {
	if cfg == nil || strings.TrimSpace(cfg.ServerURL) == "" {
		return "", "", "", localdaemon.ErrInvalidInventoryConfig
	}
	executable, err := os.Executable()
	if err != nil {
		return "", "", "", err
	}
	if runtime.GOOS == "windows" {
		// The owner-scoped SCM service uses its persisted installation authority.
		// CLI config/server overrides are not service launch arguments.
		return executable, "", "", nil
	}
	configPath := cfg.Path()
	if configPath != "" {
		configPath, err = filepath.Abs(configPath)
		if err != nil {
			return "", "", "", err
		}
	}
	return executable, configPath, cfg.ServerURL, nil
}

func waitForLocalDaemonSnapshot(ctx context.Context, client localDaemonSnapshotClient, socketPath string, lastSocketErr error) (localapi.Snapshot, error) {
	if ctx == nil || client == nil || strings.TrimSpace(socketPath) == "" {
		return localapi.Snapshot{}, localdaemon.ErrInvalidInventoryConfig
	}
	ticker := time.NewTicker(localDaemonReadyPollInterval)
	defer ticker.Stop()
	for {
		snapshot, err := client.Snapshot(ctx)
		if err == nil {
			return snapshot, nil
		}
		if contextErr := ctx.Err(); contextErr != nil {
			if lastSocketErr == nil && localDaemonSocketUnavailable(err) {
				lastSocketErr = fmt.Errorf("connect local daemon at %s: %w", socketPath, err)
			}
			if lastSocketErr != nil {
				return localapi.Snapshot{}, errors.Join(lastSocketErr, contextErr)
			}
			return localapi.Snapshot{}, errors.Join(err, contextErr)
		}
		if !localDaemonSocketUnavailable(err) {
			return localapi.Snapshot{}, err
		}
		lastSocketErr = fmt.Errorf("connect local daemon at %s: %w", socketPath, err)
		select {
		case <-ctx.Done():
			return localapi.Snapshot{}, errors.Join(lastSocketErr, ctx.Err())
		case <-ticker.C:
		}
	}
}

func acquireLocalDaemonLaunch(ctx context.Context) (func(), error) {
	select {
	case localDaemonLaunchGate <- struct{}{}:
		return func() { <-localDaemonLaunchGate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func recoverLocalDaemonSnapshot(ctx context.Context, client localDaemonSnapshotClient, socketPath string, launch localDaemonSnapshotLauncher) (localapi.Snapshot, error) {
	if ctx == nil || client == nil || launch == nil || strings.TrimSpace(socketPath) == "" {
		return localapi.Snapshot{}, localdaemon.ErrInvalidInventoryConfig
	}
	snapshot, err := client.Snapshot(ctx)
	if err == nil {
		return snapshot, nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		if localDaemonSocketUnavailable(err) {
			err = fmt.Errorf("connect local daemon at %s: %w", socketPath, err)
		}
		return localapi.Snapshot{}, errors.Join(err, contextErr)
	}
	if !localDaemonSocketUnavailable(err) {
		return localapi.Snapshot{}, err
	}
	lastSocketErr := fmt.Errorf("connect local daemon at %s: %w", socketPath, err)
	release, err := acquireLocalDaemonLaunch(ctx)
	if err != nil {
		return localapi.Snapshot{}, errors.Join(lastSocketErr, err)
	}
	defer release()

	snapshot, err = client.Snapshot(ctx)
	if err == nil {
		return snapshot, nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return localapi.Snapshot{}, errors.Join(lastSocketErr, contextErr)
	}
	if !localDaemonSocketUnavailable(err) {
		return localapi.Snapshot{}, err
	}
	lastSocketErr = fmt.Errorf("connect local daemon at %s: %w", socketPath, err)
	if err := launch(ctx); err != nil {
		return localapi.Snapshot{}, errors.Join(lastSocketErr, err)
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return localapi.Snapshot{}, errors.Join(lastSocketErr, contextErr)
	}
	return waitForLocalDaemonSnapshot(ctx, client, socketPath, lastSocketErr)
}

// startLocalDaemonAndWait first accepts an already-ready owner daemon. When
// the local endpoint is absent it installs the service and waits for one valid
// snapshot, so process creation cannot be mistaken for readiness.
func startLocalDaemonAndWait(ctx context.Context, cfg *config.Config, install localDaemonServiceInstaller) (*localapi.Client, localapi.Snapshot, error) {
	return startLocalDaemonAndWaitWithin(ctx, cfg, install, localDaemonReadyTimeout)
}

func startLocalDaemonAndWaitWithin(ctx context.Context, cfg *config.Config, install localDaemonServiceInstaller, timeout time.Duration) (*localapi.Client, localapi.Snapshot, error) {
	if install == nil {
		return nil, localapi.Snapshot{}, localdaemon.ErrInvalidInventoryConfig
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		return nil, localapi.Snapshot{}, localdaemon.ErrInvalidInventoryConfig
	}
	operationCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return nil, localapi.Snapshot{}, err
	}
	client, err := localapi.NewClient(paths.SocketPath, 2*time.Second)
	if err != nil {
		return nil, localapi.Snapshot{}, err
	}
	snapshot, err := recoverLocalDaemonSnapshot(operationCtx, client, paths.SocketPath, func(launchCtx context.Context) error {
		executable, configPath, server, launchErr := localDaemonLaunchValues(cfg)
		if launchErr != nil {
			return launchErr
		}
		return install(launchCtx, executable, configPath, server)
	})
	if err != nil {
		return nil, localapi.Snapshot{}, err
	}
	return client, snapshot, nil
}

// ensureLocalDaemonService is used by auth. Before the first setup it is a
// deliberate no-op; setup owns the first machine-bound daemon launch.
func ensureLocalDaemonService(ctx context.Context, cfg *config.Config) error {
	matches, err := localDaemonRegistrationMatches(cfg)
	if err != nil || !matches {
		return err
	}
	_, _, err = startLocalDaemonAndWait(ctx, cfg, installLocalDaemonService)
	return err
}

// requireLocalDaemonService is the bounded recovery path for peer commands.
// It covers reboot followed only by a non-interactive Windows OpenSSH logon,
// where the managed local daemon service is temporarily cold.
func requireLocalDaemonService(ctx context.Context, cfg *config.Config) error {
	matches, err := localDaemonRegistrationMatches(cfg)
	if err != nil {
		return err
	}
	if !matches {
		return errors.New("this machine is not set up for the active Paperboat server; run `pb setup`")
	}
	_, _, err = startLocalDaemonAndWait(ctx, cfg, installLocalDaemonService)
	return err
}

func localDaemonSnapshot(command *cobra.Command, install localDaemonServiceInstaller) (*localapi.Client, localapi.Snapshot, error) {
	if command == nil || install == nil {
		return nil, localapi.Snapshot{}, errors.New("invalid local daemon client configuration")
	}
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return nil, localapi.Snapshot{}, err
	}
	client, err := localapi.NewClient(paths.SocketPath, 2*time.Second)
	if err != nil {
		return nil, localapi.Snapshot{}, err
	}
	commandCtx := command.Context()
	if commandCtx == nil {
		commandCtx = context.Background()
	}
	operationCtx, cancel := context.WithTimeout(commandCtx, localDaemonReadyTimeout)
	defer cancel()
	snapshot, err := recoverLocalDaemonSnapshot(operationCtx, client, paths.SocketPath, func(launchCtx context.Context) error {
		executable, executableErr := os.Executable()
		if executableErr != nil {
			return executableErr
		}
		configPath := configPathFlag(command)
		effectiveConfig, configErr := config.Load(configPath)
		if configErr != nil {
			return configErr
		}
		configPath = effectiveConfig.Path()
		if configPath != "" {
			configPath, executableErr = filepath.Abs(configPath)
			if executableErr != nil {
				return executableErr
			}
		}
		server, _ := command.Flags().GetString("server")
		if strings.TrimSpace(server) == "" {
			server = effectiveConfig.ServerURL
		}
		return install(launchCtx, executable, configPath, server)
	})
	if err != nil {
		return nil, localapi.Snapshot{}, err
	}
	return client, snapshot, nil
}

func writeWaitResult(stdout, stderr io.Writer, result localwait.Result) {
	switch result.Outcome {
	case "ready":
		fmt.Fprintf(stdout, "%s is ready for %s (%s, generation %d).\n", result.Machine.Alias, result.Condition, result.Machine.RuntimeState, result.SnapshotGeneration)
	case "timeout":
		fmt.Fprintf(stderr, "pb: Timed out waiting for %s to become ready for %s.\n", result.Machine.Alias, result.Condition)
	case "canceled":
		fmt.Fprintln(stderr, "pb: Operation canceled.")
	case "failed":
		fmt.Fprintf(stderr, "pb: %s cannot become ready for %s (%s).\n", result.Machine.Alias, result.Condition, result.Code)
	}
}

func waitExitCode(result localwait.Result) int {
	switch result.Outcome {
	case "ready":
		return 0
	case "timeout":
		return 203
	case "canceled":
		return 205
	default:
		return 1
	}
}
