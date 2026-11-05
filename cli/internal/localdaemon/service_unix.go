//go:build darwin || linux

package localdaemon

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/daemonrpc"
	"github.com/pinksaucepasta/paperboat/internal/endpointbinary"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	hostservice "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

type serviceConfig struct {
	Platform    string
	Home        string
	Executable  string
	Generation  string
	ConfigPath  string
	ServerURL   string
	Username    string
	Group       string
	UID         int
	Environment map[string]string
	Runner      hostservice.Runner
}

func InstallCurrentUserService(ctx context.Context, executable, configPath, serverURL string) error {
	resolved, err := endpointbinary.Daemon(executable)
	if err != nil {
		return err
	}
	config, err := currentUserServiceConfig(executable)
	if err != nil {
		return err
	}
	if config.Generation, err = DaemonGeneration(resolved); err != nil {
		return err
	}
	config.Executable = resolved
	config.ConfigPath, config.ServerURL = configPath, serverURL
	return installService(ctx, config)
}

// ReplaceCurrentUserService starts the supplied binary's daemon generation in its own unit, then
// sends SIGQUIT to the generations before it: they hand over the lock, sockets and guard and keep
// draining their forwarded connections (make-before-break; nginx binary upgrade, Teleport graceful
// restart lib/service/signals.go). Rollback removes the new unit and restarts the newest previous
// generation; its drained streams end, but the computer has a serving daemon again.
func ReplaceCurrentUserService(ctx context.Context, executable, configPath, serverURL, version string) (func(context.Context) error, error) {
	resolved, err := endpointbinary.Daemon(executable)
	if err != nil {
		return nil, err
	}
	config, err := currentUserServiceConfig(executable)
	if err != nil {
		return nil, err
	}
	if config.Generation, err = DaemonGeneration(resolved); err != nil {
		return nil, err
	}
	config.Executable, config.ConfigPath, config.ServerURL = resolved, configPath, serverURL
	return replaceService(ctx, config, version)
}

func replaceService(ctx context.Context, config serviceConfig, version string) (func(context.Context) error, error) {
	installer, err := newServiceInstaller(config)
	if err != nil {
		return nil, err
	}
	// Supplied-install validation already activated this version. Preserve that
	// process while still activating any changed service declaration.
	if version != "" {
		probe, probeErr := ProbeCurrentUserForUpdate(ctx)
		if probeErr == nil && probe.Running && probe.Version == version && probe.State != "starting" && probe.State != "stopping" {
			return installer.Reconcile(ctx)
		}
	}
	previous, err := installedGenerations(config.Platform, config.Home)
	if err != nil {
		return nil, err
	}
	restoreNew, err := installer.Replace(ctx)
	if restoreNew == nil {
		return nil, err
	}
	restore := func(recovery context.Context) error {
		return errors.Join(restoreNew(recovery), restartGeneration(recovery, config, previous))
	}
	if err != nil {
		return restore, err
	}
	// The new process waits for the daemon lock, which the old one releases on SIGQUIT.
	_, err = quitOtherGenerations(ctx, config, config.Generation)
	return restore, err
}

// restartGeneration starts a fresh process of the newest generation in previous other than the one
// being rolled back, ending a drain it may still run.
func restartGeneration(ctx context.Context, config serviceConfig, previous []string) error {
	for _, generation := range previous {
		if generation == config.Generation {
			continue
		}
		controller, err := hostservice.DaemonController(config.Platform, generation, config.UID, config.Runner)
		if err != nil {
			return err
		}
		native := controller.(hostservice.NativeLifecycleController)
		path := generationDefinitionPath(config.Platform, config.Home, generation)
		return errors.Join(native.Stop(ctx, path), native.Start(ctx, path))
	}
	return nil
}

func RemoveCurrentUserService(ctx context.Context, executable string) error {
	config, err := currentUserServiceConfig(executable)
	if err != nil {
		return err
	}
	if err := removeService(ctx, config); err != nil {
		return err
	}
	return daemonrpc.CleanupUnusedAddresses(ctx)
}

// removeService also removes the config sync service `pb config` installs beside the daemon: it
// belongs to the same enrollment, and left behind it crash-looped on the missing registration after
// pb uninstall or pb reset (macOS, 2026-11-04).
func removeService(ctx context.Context, config serviceConfig) error {
	generations, err := installedGenerations(config.Platform, config.Home)
	if err != nil {
		return err
	}
	for _, generation := range generations {
		config.Generation = generation
		definition, err := serviceDefinition(config)
		if err != nil {
			return err
		}
		if err := hostservice.Remove(ctx, definition); err != nil {
			return err
		}
	}
	if !filepath.IsAbs(config.Home) || !filepath.IsAbs(config.Executable) || config.Username == "" || config.Group == "" || config.UID < 0 || config.Runner == nil {
		return ErrInvalidInventoryConfig
	}
	definition := hostservice.Config{
		Platform: config.Platform, Kind: hostservice.ConfigKind, ConfigRoot: config.Home, Executable: config.Executable,
		User: config.Username, Group: config.Group, Arguments: []string{"daemon"}, Environment: config.Environment,
	}
	if config.Platform == "darwin" {
		definition.Controller = hostservice.LaunchdController{Runner: config.Runner, UID: config.UID, Label: hostservice.ConfigLabel, UserDomain: true}
	} else {
		definition.Controller = hostservice.SystemdController{Runner: config.Runner, Unit: hostservice.ConfigUnit, User: true}
	}
	return hostservice.Remove(ctx, definition)
}

func currentUserServiceConfig(executable string) (serviceConfig, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return serviceConfig{}, err
	}
	account, err := user.Current()
	if err != nil {
		return serviceConfig{}, err
	}
	group, err := user.LookupGroupId(account.Gid)
	if err != nil {
		return serviceConfig{}, err
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return serviceConfig{}, err
	}
	resolvedExecutable, err := endpointbinary.DaemonPathForRemoval(executable)
	if err != nil {
		return serviceConfig{}, err
	}
	environment := map[string]string{"HOME": home}
	for _, key := range []string{"XDG_STATE_HOME", "XDG_RUNTIME_DIR", "TMPDIR", "PAPERBOAT_RUNTIME_STATE_ROOT"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" && filepath.IsAbs(value) {
			environment[key] = filepath.Clean(value)
		}
	}
	return serviceConfig{
		Platform: runtime.GOOS, Home: home, Executable: resolvedExecutable,
		Username: account.Username, Group: group.Name, UID: uid,
		Environment: environment, Runner: hostservice.ExecRunner{},
	}, nil
}

func installService(ctx context.Context, config serviceConfig) error {
	if ctx == nil || !filepath.IsAbs(config.Home) || !filepath.IsAbs(config.Executable) || config.Username == "" || config.Group == "" || config.UID < 0 || config.Runner == nil || config.ConfigPath != "" && !filepath.IsAbs(config.ConfigPath) {
		return ErrInvalidInventoryConfig
	}
	installer, err := newServiceInstaller(config)
	if err != nil {
		return err
	}
	if err := installer.Install(ctx); err != nil {
		return errors.Join(errors.New("install Paperboat local daemon service"), err)
	}
	return nil
}

func newServiceInstaller(config serviceConfig) (*hostservice.Installer, error) {
	definition, err := serviceDefinition(config)
	if err != nil {
		return nil, err
	}
	return hostservice.New(definition)
}

func serviceDefinition(config serviceConfig) (hostservice.Config, error) {
	if !filepath.IsAbs(config.Home) || !filepath.IsAbs(config.Executable) || config.Username == "" || config.Group == "" || config.UID < 0 || config.Runner == nil || config.ConfigPath != "" && !filepath.IsAbs(config.ConfigPath) {
		return hostservice.Config{}, ErrInvalidInventoryConfig
	}
	arguments := []string{"daemon", "--generation", config.Generation}
	if config.ConfigPath != "" {
		arguments = append(arguments, "--config", filepath.Clean(config.ConfigPath))
	}
	if strings.TrimSpace(config.ServerURL) != "" {
		arguments = append(arguments, "--server", strings.TrimSpace(config.ServerURL))
	}
	if config.Platform != "darwin" && config.Platform != "linux" {
		return hostservice.Config{}, hostservice.ErrUnsupportedPlatform
	}
	controller, err := hostservice.DaemonController(config.Platform, config.Generation, config.UID, config.Runner)
	if err != nil {
		return hostservice.Config{}, err
	}
	return hostservice.Config{
		Platform: config.Platform, Kind: hostservice.DaemonKind, Generation: config.Generation, ConfigRoot: config.Home,
		Executable: config.Executable, User: config.Username, Group: config.Group,
		Arguments: arguments, Environment: config.Environment, Controller: controller,
	}, nil
}
