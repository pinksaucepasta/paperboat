//go:build darwin || linux

package localdaemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/unit"
	"github.com/pinksaucepasta/paperboat/internal/endpointbinary"
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
	// Health is the codes of the daemon's health items, so a failed install names the step that failed.
	Health []string `json:"health,omitempty"`
	// HealthTitles are the same items' titles (they name the failing step, e.g. "the machine names step failed").
	HealthTitles []string `json:"health_titles,omitempty"`
}

// ProbeCurrentUserForUpdate runs as the enrolled user, preserving local API
// authorization. Read the installed declaration rather than inheriting the
// privileged updater's HOME, TMPDIR or XDG namespace.
func ProbeCurrentUserForUpdate(ctx context.Context) (UpdateProbe, error) {
	result := UpdateProbe{Schema: UpdateProbeSchema}
	client, err := currentUserDaemonClient(ctx)
	if err != nil || client == nil {
		return result, err
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return result, err
	}
	result.Version, result.State, result.Running = snapshot.DaemonVersion, snapshot.DaemonState, true
	result.Machines = uint32(len(snapshot.Machines))
	result.ControlPlaneUnavailableOnly = len(snapshot.Health) == 1 && snapshot.Health[0].Code == "control_plane_unavailable"
	for _, item := range snapshot.Health {
		result.Health = append(result.Health, item.Code)
		result.HealthTitles = append(result.HealthTitles, item.Title)
	}
	return result, nil
}

// OpenConnectionsCurrentUser reports the SSH-family streams and access forwards the enrolled user's
// running daemon carries, which an update would end. A daemon that is not running carries none. A
// daemon too old to know the question is reported as carrying none: the answer must never block an
// update that no connection can be shown to be blocking.
func OpenConnectionsCurrentUser(ctx context.Context) (localapi.OpenConnections, error) {
	client, err := currentUserDaemonClient(ctx)
	if err != nil || client == nil {
		return localapi.OpenConnections{Schema: localapi.OpenConnectionsSchemaV1}, err
	}
	connections, err := client.OpenConnections(ctx)
	var remote *localapi.RemoteError
	if errors.As(err, &remote) && remote.StatusCode == http.StatusNotFound {
		return localapi.OpenConnections{Schema: localapi.OpenConnectionsSchemaV1}, nil
	}
	return connections, err
}

// currentUserDaemonClient returns a local API client for the enrolled user's daemon, or nil when it
// is not installed or not running.
func currentUserDaemonClient(ctx context.Context) (*localapi.Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	environment, err := updateServiceEnvironment(runtime.GOOS, home, os.Geteuid())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	controller, err := updateDaemonController(home)
	if err != nil {
		return nil, err
	}
	status, err := controller.Inspect(ctx, "")
	if err != nil || !status.Running {
		return nil, err
	}
	paths, err := localapi.ResolvePaths(func(key string) string { return environment[key] }, home, os.Geteuid())
	if err != nil {
		return nil, err
	}
	return localapi.NewClient(paths.SocketPath, time.Second)
}

// RestartCurrentUserForUpdate starts the updated binary's daemon generation in its own unit, with the
// arguments and environment of the generation the last install started, then sends the others
// SIGQUIT so they drain (replaceService). The updater runs it as the enrolled user after promotion.
func RestartCurrentUserForUpdate(ctx context.Context) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	declared, err := readUpdateDeclaration(runtime.GOOS, home, os.Geteuid())
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	resolved, err := endpointbinary.Daemon(executable)
	if err != nil {
		return err
	}
	config, err := currentUserServiceConfig(resolved)
	if err != nil {
		return err
	}
	if config.Generation, err = DaemonGeneration(resolved); err != nil {
		return err
	}
	config.Executable, config.Environment = resolved, declared.environment
	for i := 0; i+1 < len(declared.arguments); i++ {
		switch declared.arguments[i] {
		case "--config":
			config.ConfigPath = declared.arguments[i+1]
		case "--server":
			config.ServerURL = declared.arguments[i+1]
		}
	}
	_, err = replaceService(ctx, config, "")
	return err
}

// updateDaemonController addresses the newest installed daemon generation, the one the last install started.
func updateDaemonController(home string) (service.NativeLifecycleController, error) {
	generations, err := installedGenerations(runtime.GOOS, home)
	if err != nil {
		return nil, err
	}
	if len(generations) == 0 {
		return nil, os.ErrNotExist
	}
	controller, err := service.DaemonController(runtime.GOOS, generations[0], os.Geteuid(), service.ExecRunner{})
	if err != nil {
		return nil, err
	}
	return controller.(service.NativeLifecycleController), nil
}

// updateDeclaration is what the newest daemon generation's definition declares.
type updateDeclaration struct {
	environment map[string]string
	arguments   []string // after the executable
}

func updateServiceEnvironment(platform, home string, uid int) (map[string]string, error) {
	declared, err := readUpdateDeclaration(platform, home, uid)
	if err != nil {
		return nil, err
	}
	values := declared.environment
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

// updateServiceDefinitionPath is the newest installed daemon generation's definition, or a path
// that does not exist when none is installed.
func updateServiceDefinitionPath(platform, home string) string {
	generations, _ := installedGenerations(platform, home)
	if len(generations) == 0 {
		return generationDefinitionPath(platform, home, "none")
	}
	return generationDefinitionPath(platform, home, generations[0])
}

func readUpdateDeclaration(platform, home string, uid int) (updateDeclaration, error) {
	path := updateServiceDefinitionPath(platform, home)
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return updateDeclaration{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return updateDeclaration{}, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != uid || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() < 1 || info.Size() > 64<<10 {
		return updateDeclaration{}, localapi.ErrUnsafeSocket
	}
	body, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil || len(body) > 64<<10 {
		return updateDeclaration{}, errors.Join(localapi.ErrInvalidConfig, err)
	}
	declared := updateDeclaration{environment: map[string]string{}}
	if platform == "darwin" {
		var declaration struct {
			Environment map[string]string `plist:"EnvironmentVariables"`
			Arguments   []string          `plist:"ProgramArguments"`
		}
		if _, err := plist.Unmarshal(body, &declaration); err != nil {
			return updateDeclaration{}, err
		}
		if declaration.Environment != nil {
			declared.environment = declaration.Environment
		}
		if len(declaration.Arguments) > 0 {
			declared.arguments = declaration.Arguments[1:]
		}
		return declared, nil
	}
	options, err := unit.Deserialize(bytes.NewReader(body))
	if err != nil {
		return updateDeclaration{}, err
	}
	for _, option := range options {
		if option.Section != "Service" {
			continue
		}
		switch option.Name {
		case "Environment":
			value, err := unsystemdQuote(option.Value)
			if err != nil {
				return updateDeclaration{}, err
			}
			key, value, ok := strings.Cut(value, "=")
			if !ok {
				return updateDeclaration{}, localapi.ErrInvalidConfig
			}
			declared.environment[key] = value
		case "ExecStart":
			// service.renderSystemd writes each word as one systemdEscape'd quoted string.
			for rest := strings.TrimSpace(option.Value); rest != ""; rest = strings.TrimSpace(rest) {
				quoted, err := strconv.QuotedPrefix(rest)
				if err != nil {
					return updateDeclaration{}, localapi.ErrInvalidConfig
				}
				word, err := unsystemdQuote(quoted)
				if err != nil {
					return updateDeclaration{}, err
				}
				declared.arguments = append(declared.arguments, word)
				rest = rest[len(quoted):]
			}
			if len(declared.arguments) > 0 {
				declared.arguments = declared.arguments[1:]
			}
		}
	}
	return declared, nil
}

// unsystemdQuote reverses service.systemdEscape.
func unsystemdQuote(value string) (string, error) {
	unquoted, err := strconv.Unquote(value)
	if err != nil {
		return "", localapi.ErrInvalidConfig
	}
	return strings.ReplaceAll(strings.ReplaceAll(unquoted, "%%", "%"), "$$", "$"), nil
}
