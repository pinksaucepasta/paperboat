//go:build darwin || linux

package localdaemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/unit"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"howett.net/plist"
)

const UpdateProbeSchema = "paperboat.update-probe/v1"

// UpdateProbe is the minimum authenticated daemon observation needed by the
// privileged updater. Machine identities and user content never cross this IPC.
type UpdateProbe struct {
	Schema                      string `json:"schema"`
	Version                     string `json:"version"`
	UpdaterVersion              string `json:"updater_version"`
	State                       string `json:"state"`
	Machines                    uint32 `json:"machines"`
	ControlPlaneUnavailableOnly bool   `json:"control_plane_unavailable_only"`
	Running                     bool   `json:"running"`
}

// ProbeCurrentUserForUpdate runs as the enrolled user, preserving local API
// authorization. Read the installed declaration rather than inheriting the
// privileged updater's HOME, TMPDIR or XDG namespace.
func ProbeCurrentUserForUpdate(ctx context.Context) (UpdateProbe, error) {
	result := UpdateProbe{Schema: UpdateProbeSchema}
	home, err := os.UserHomeDir()
	if err != nil {
		return result, err
	}
	environment, err := updateServiceEnvironment(runtime.GOOS, home, os.Geteuid())
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	status, err := updateDaemonController().Inspect(ctx, "")
	if err != nil || !status.Running {
		return result, err
	}
	paths, err := localapi.ResolvePaths(func(key string) string { return environment[key] }, home, os.Geteuid())
	if err != nil {
		return result, err
	}
	client, err := localapi.NewClient(paths.SocketPath, time.Second)
	if err != nil {
		return result, err
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return result, err
	}
	result.Version, result.State, result.Running = snapshot.DaemonVersion, snapshot.DaemonState, true
	result.Machines = uint32(len(snapshot.Machines))
	result.ControlPlaneUnavailableOnly = len(snapshot.Health) == 1 && snapshot.Health[0].Code == "control_plane_unavailable"
	return result, nil
}

func RestartCurrentUserForUpdate(ctx context.Context) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if _, err := updateServiceEnvironment(runtime.GOOS, home, os.Geteuid()); err != nil {
		return err
	}
	controller := updateDaemonController()
	if err := controller.Stop(ctx, ""); err != nil {
		return err
	}
	return controller.Start(ctx, updateServiceDefinitionPath(runtime.GOOS, home))
}

func updateDaemonController() service.NativeLifecycleController {
	if runtime.GOOS == "darwin" {
		return service.LaunchdController{Runner: service.ExecRunner{}, UID: os.Geteuid(), Label: service.DaemonLabel, UserDomain: true}
	}
	return service.SystemdController{Runner: service.ExecRunner{}, Unit: "paperboatd.service", User: true}
}

func updateServiceEnvironment(platform, home string, uid int) (map[string]string, error) {
	path := updateServiceDefinitionPath(platform, home)
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != uid || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() < 1 || info.Size() > 64<<10 {
		return nil, localapi.ErrUnsafeSocket
	}
	body, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil || len(body) > 64<<10 {
		return nil, errors.Join(localapi.ErrInvalidConfig, err)
	}
	values := map[string]string{}
	if platform == "darwin" {
		var declaration struct {
			Environment map[string]string `plist:"EnvironmentVariables"`
		}
		if _, err := plist.Unmarshal(body, &declaration); err != nil {
			return nil, err
		}
		values = declaration.Environment
	} else {
		options, err := unit.Deserialize(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		for _, option := range options {
			if option.Section != "Service" || option.Name != "Environment" {
				continue
			}
			value, err := strconv.Unquote(option.Value)
			if err != nil {
				return nil, localapi.ErrInvalidConfig
			}
			value = strings.ReplaceAll(strings.ReplaceAll(value, "%%", "%"), "$$", "$")
			key, value, ok := strings.Cut(value, "=")
			if !ok {
				return nil, localapi.ErrInvalidConfig
			}
			values[key] = value
		}
	}
	environment := map[string]string{"HOME": home}
	for _, key := range []string{"XDG_STATE_HOME", "XDG_RUNTIME_DIR", "TMPDIR"} {
		if value := values[key]; value != "" {
			if !filepath.IsAbs(value) || filepath.Clean(value) != value || strings.ContainsAny(value, "\x00\r\n") {
				return nil, localapi.ErrInvalidConfig
			}
			environment[key] = value
		}
	}
	return environment, nil
}

func updateServiceDefinitionPath(platform, home string) string {
	if platform == "darwin" {
		return filepath.Join(home, "Library", "LaunchAgents", service.DaemonLabel+".plist")
	}
	return filepath.Join(home, ".config", "systemd", "user", "paperboatd.service")
}
