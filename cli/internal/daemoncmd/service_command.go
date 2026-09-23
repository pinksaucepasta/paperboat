package daemoncmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
	"github.com/spf13/cobra"
)

const daemonServiceName = "paperboatd"

var (
	installDaemonService = localdaemon.InstallCurrentUserService
	removeDaemonService  = localdaemon.UninstallCurrentUserService
	startDaemonService   = localdaemon.StartCurrentUserService
	stopDaemonService    = localdaemon.StopCurrentUserService
	inspectDaemonService = localdaemon.InspectCurrentUserService
	serviceExecutable    = os.Executable
	servicePaths         = localdaemon.CurrentUserPaths
	newLocalAPIClient    = localapi.NewClient
	awaitDaemonReady     = waitDaemonReady
	awaitDaemonStopped   = waitDaemonStopped
)

func ServiceCommand() *cobra.Command { return serviceCommand() }
func serviceCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "service", Short: "Manage the Paperboat background daemon service"}
	cmd.AddCommand(serviceInstallCommand(), serviceUninstallCommand(), serviceStartCommand(), serviceStopCommand(), serviceRestartCommand(), serviceStatusCommand())
	return cmd
}
func serviceContext(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	return context.WithTimeout(cmd.Context(), 30*time.Second)
}
func executableForService() (string, error) {
	executable, err := serviceExecutable()
	if err != nil {
		return "", err
	}
	return filepath.Abs(executable)
}
func waitDaemonReady(ctx context.Context) error {
	paths, err := servicePaths()
	if err != nil {
		return err
	}
	client, err := newLocalAPIClient(paths.SocketPath, time.Second)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	lastState := "unavailable"
	var lastErr error
	for {
		snapshot, snapshotErr := client.Snapshot(ctx)
		if snapshotErr == nil {
			lastState, lastErr = snapshot.DaemonState, nil
			if snapshot.DaemonState == "ready" {
				return nil
			}
		} else {
			lastErr = snapshotErr
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("Paperboat local daemon did not become ready; last local status check failed: %v; run pb status and retry: %w", lastErr, ctx.Err())
			}
			return fmt.Errorf("Paperboat local daemon did not become ready; last state was %q; run pb status and retry: %w", lastState, ctx.Err())
		case <-ticker.C:
		}
	}
}
func waitDaemonStopped(ctx context.Context, executable string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := inspectDaemonService(ctx, executable)
		if err != nil {
			return err
		}
		if !state.Running {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("Paperboat local daemon did not stop: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
func serviceInstallCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "install", Short: "Install Paperboat daemon as the current-user service", Args: commandArgs(cobra.NoArgs)}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		configPath, _ := cmd.Flags().GetString("config")
		serverURL, _ := cmd.Flags().GetString("server")
		if configPath != "" && !filepath.IsAbs(configPath) {
			return errors.New("service configuration path must be absolute")
		}
		executable, err := executableForService()
		if err != nil {
			return err
		}
		ctx, cancel := serviceContext(cmd)
		defer cancel()
		if err = installDaemonService(ctx, executable, configPath, serverURL); err != nil {
			return fmt.Errorf("install Paperboat local daemon service: %w", err)
		}
		if err = awaitDaemonReady(ctx); err != nil {
			return err
		}
		return writeDaemonCommandResult(cmd, map[string]any{"service": daemonServiceName, "action": "install", "completed": true}, "Paperboat daemon service installed and ready")
	}
	cmd.Flags().Bool("json", false, "print JSON")
	cmd.Flags().String("config", "", "configuration file path")
	cmd.Flags().String("server", "", "Paperboat server URL")
	return cmd
}
func serviceUninstallCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "uninstall", Short: "Uninstall Paperboat daemon current-user service", Args: commandArgs(cobra.NoArgs)}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		executable, err := executableForService()
		if err != nil {
			return err
		}
		ctx, cancel := serviceContext(cmd)
		defer cancel()
		if err = removeDaemonService(ctx, executable); err != nil {
			return fmt.Errorf("uninstall Paperboat local daemon service: %w", err)
		}
		return writeDaemonCommandResult(cmd, map[string]any{"service": daemonServiceName, "action": "uninstall", "completed": true}, "Paperboat daemon service uninstalled")
	}
	cmd.Flags().Bool("json", false, "print JSON")
	return cmd
}
func serviceStartCommand() *cobra.Command {
	return serviceActionCommand("start", "Start Paperboat daemon current-user service", true)
}
func serviceStopCommand() *cobra.Command {
	return serviceActionCommand("stop", "Stop Paperboat daemon current-user service", false)
}
func serviceActionCommand(action, short string, ready bool) *cobra.Command {
	cmd := &cobra.Command{Use: action, Short: short, Args: commandArgs(cobra.NoArgs)}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		executable, err := executableForService()
		if err != nil {
			return err
		}
		ctx, cancel := serviceContext(cmd)
		defer cancel()
		if ready {
			err = startDaemonService(ctx, executable)
		} else {
			err = stopDaemonService(ctx, executable)
		}
		if err != nil {
			return fmt.Errorf("%s Paperboat local daemon service: %w", action, err)
		}
		if ready {
			err = awaitDaemonReady(ctx)
		} else {
			err = awaitDaemonStopped(ctx, executable)
		}
		if err != nil {
			return err
		}
		message := "Paperboat daemon service stopped"
		if ready {
			message = "Paperboat daemon service started and ready"
		}
		return writeDaemonCommandResult(cmd, map[string]any{"service": daemonServiceName, "action": action, "completed": true}, message)
	}
	cmd.Flags().Bool("json", false, "print JSON")
	return cmd
}
func serviceRestartCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "restart", Short: "Restart Paperboat daemon current-user service", Args: commandArgs(cobra.NoArgs)}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		executable, err := executableForService()
		if err != nil {
			return err
		}
		ctx, cancel := serviceContext(cmd)
		defer cancel()
		if err = stopDaemonService(ctx, executable); err != nil {
			return fmt.Errorf("stop Paperboat local daemon service: %w", err)
		}
		if err = awaitDaemonStopped(ctx, executable); err != nil {
			return err
		}
		if err = startDaemonService(ctx, executable); err != nil {
			return fmt.Errorf("start Paperboat local daemon service: %w", err)
		}
		if err = awaitDaemonReady(ctx); err != nil {
			return err
		}
		return writeDaemonCommandResult(cmd, map[string]any{"service": daemonServiceName, "action": "restart", "completed": true}, "Paperboat daemon service restarted and ready")
	}
	cmd.Flags().Bool("json", false, "print JSON")
	return cmd
}
func serviceStatusCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "status", Short: "Show Paperboat daemon current-user service status", Args: commandArgs(cobra.NoArgs)}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		executable, err := executableForService()
		if err != nil {
			return err
		}
		ctx, cancel := serviceContext(cmd)
		defer cancel()
		state, err := inspectDaemonService(ctx, executable)
		if err != nil {
			return fmt.Errorf("read Paperboat local daemon service status: %w", err)
		}
		status := "not-installed"
		if state.Installed {
			status = "stopped"
		}
		if state.Running {
			status = "running"
		}
		return writeDaemonCommandResult(cmd, map[string]any{"service": daemonServiceName, "status": status}, fmt.Sprintf("Paperboat daemon service status: %s", status))
	}
	cmd.Flags().Bool("json", false, "print JSON")
	return cmd
}
func runDaemonCommand() *cobra.Command {
	cmd := localDaemonCommand()
	cmd.Use = "run"
	cmd.Short = "Run Paperboat daemon under service supervision"
	cmd.Hidden = false
	return cmd
}
