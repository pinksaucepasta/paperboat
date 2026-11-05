//go:build darwin || linux

package localdaemon

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	hostservice "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

// ErrSingleUnitDaemon refuses an install or update over the single paperboatd unit that predates
// per-generation units: that daemon cannot hand over, so nothing is started or stopped and the
// install rolls back, leaving it running. No migration: the user reinstalls (zero compatibility).
var ErrSingleUnitDaemon = errors.New("this computer still runs the earlier single Paperboat daemon service, which cannot hand over to this version; the previous installation is left running. Reinstall Paperboat: run `pb uninstall`, then install it again")

// refuseSingleUnitDaemon fails with ErrSingleUnitDaemon while that earlier unit is installed.
func refuseSingleUnitDaemon(platform, home string) error {
	path := filepath.Join(home, ".config", "systemd", "user", "paperboatd.service")
	if platform == "darwin" {
		path = filepath.Join(home, "Library", "LaunchAgents", hostservice.DaemonLabel+".plist")
	}
	if _, err := os.Lstat(path); err == nil {
		return ErrSingleUnitDaemon
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func generationDefinitionPath(platform, home, generation string) string {
	if platform == "darwin" {
		return filepath.Join(home, "Library", "LaunchAgents", hostservice.DaemonLabel+"."+generation+".plist")
	}
	return filepath.Join(home, ".config", "systemd", "user", hostservice.DaemonUnitPrefix+generation+".service")
}

// installedGenerations lists the daemon generations with a definition in home, newest first: the
// newest definition is the generation the last install started.
func installedGenerations(platform, home string) ([]string, error) {
	pattern := generationDefinitionPath(platform, home, "*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	prefix, suffix, _ := strings.Cut(filepath.Base(pattern), "*")
	type entry struct {
		generation string
		modified   int64
	}
	var entries []entry
	for _, match := range matches {
		generation := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(match), prefix), suffix)
		info, statErr := os.Lstat(match)
		if statErr != nil || !info.Mode().IsRegular() || !validGeneration(generation) {
			continue
		}
		entries = append(entries, entry{generation, info.ModTime().UnixNano()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].modified > entries[j].modified })
	generations := make([]string, len(entries))
	for i, e := range entries {
		generations[i] = e.generation
	}
	return generations, nil
}

func validGeneration(value string) bool {
	if len(value) != 32 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// quitOtherGenerations sends SIGQUIT to every running daemon generation but keep. Each releases its
// lock, sockets and guard connection, so the kept one takes over, and drains its forwarded streams.
func quitOtherGenerations(ctx context.Context, config serviceConfig, keep string) ([]string, error) {
	generations, err := installedGenerations(config.Platform, config.Home)
	if err != nil {
		return nil, err
	}
	var quit []string
	var failures error
	for _, generation := range generations {
		if generation == keep {
			continue
		}
		controller, err := hostservice.DaemonController(config.Platform, generation, config.UID, config.Runner)
		if err != nil {
			return quit, err
		}
		state, err := controller.(hostservice.NativeLifecycleController).Inspect(ctx, generationDefinitionPath(config.Platform, config.Home, generation))
		if err != nil || !state.Running {
			failures = errors.Join(failures, err)
			continue
		}
		if err := hostservice.QuitCarrier(ctx, controller); err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		quit = append(quit, generation)
	}
	return quit, failures
}

// PruneExitedGenerations removes the units of daemon generations other than current that no longer
// run: a drained generation exits 0 and its unit stays down until the live generation removes it.
func PruneExitedGenerations(ctx context.Context, executable, current string) error {
	config, err := currentUserServiceConfig(executable)
	if err != nil {
		return err
	}
	return pruneExitedGenerations(ctx, config, current)
}

func pruneExitedGenerations(ctx context.Context, config serviceConfig, current string) error {
	generations, err := installedGenerations(config.Platform, config.Home)
	if err != nil {
		return err
	}
	var failures error
	for _, generation := range generations {
		if generation == current {
			continue
		}
		config.Generation = generation
		definition, err := serviceDefinition(config)
		if err != nil {
			return err
		}
		state, err := definition.Controller.(hostservice.NativeLifecycleController).Inspect(ctx, generationDefinitionPath(config.Platform, config.Home, generation))
		if err != nil || state.Running {
			failures = errors.Join(failures, err)
			continue
		}
		failures = errors.Join(failures, hostservice.Remove(ctx, definition))
	}
	return failures
}
