//go:build linux || darwin

package hostinstall

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

type cutoverRunner struct{ stopped bool }

func (r *cutoverRunner) Run(_ context.Context, _ string, args ...string) error {
	for _, arg := range args {
		if arg == "stop" {
			r.stopped = true
		}
	}
	return nil
}
func (r *cutoverRunner) Output(ctx context.Context, command string, args ...string) (string, error) {
	return "", r.Run(ctx, command, args...)
}

func TestInvalidReplacementLeavesServingRuntimeRunning(t *testing.T) {
	runner := &cutoverRunner{}
	controller, err := service.ComponentController("linux", service.HostdKind, 1001, runner)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	hostd, err := service.NewPending(service.Config{
		Platform: "linux", Kind: service.HostdKind, Instance: "u1001", ConfigRoot: root,
		Executable: filepath.Join(root, "pb"), User: "tester", Group: "tester",
		Arguments: []string{"daemon", "__runtime-hostd"}, Controller: controller,
	})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := installsource.Inspect(executable, "dev-cutover", installsource.Custom)
	if err != nil {
		t.Fatal(err)
	}
	admissionClosed := false
	err = stageAndStopUnixRuntimeRoles(t.Context(),
		Request{Executable: filepath.Join(root, "missing"), Source: source},
		installPaths{workerNext: filepath.Join(root, "next")}, nil, hostd, nil,
		func(context.Context) error { admissionClosed = true; return nil })
	if err == nil || runner.stopped || admissionClosed {
		t.Fatalf("invalid candidate interrupted serving runtime: stopped=%v admissionClosed=%v err=%v", runner.stopped, admissionClosed, err)
	}
	if _, err = os.Stat(filepath.Join(root, "next")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid candidate was published", err)
	}
}

type powerFailureRunner struct{ hostdStarted bool }

func (r *powerFailureRunner) Run(_ context.Context, _ string, args ...string) error {
	if len(args) >= 2 && args[0] == "start" {
		if args[1] == "power.service" {
			return errors.New("power policy unavailable")
		}
		if args[1] == "hostd.service" {
			r.hostdStarted = true
		}
	}
	return nil
}
func (r *powerFailureRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	return "", r.Run(ctx, name, args...)
}

func TestRuntimeStartsBeforeIndependentPowerPolicy(t *testing.T) {
	runner := &powerFailureRunner{}
	root := t.TempDir()
	makeRole := func(kind, unit string) *service.Installer {
		t.Helper()
		installer, err := service.NewPending(service.Config{Platform: "linux", Kind: kind, Instance: "u1001", ConfigRoot: root, Executable: filepath.Join(root, "pb"), User: "tester", Group: "tester", Arguments: []string{"daemon"}, Controller: service.SystemdController{Runner: runner, Unit: unit}})
		if err != nil {
			t.Fatal(err)
		}
		return installer
	}
	host, hostd := makeRole(service.HostKind, "power.service"), makeRole(service.HostdKind, "hostd.service")
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"live":true}`))
	}))
	defer health.Close()
	err := applyUnixNativeRoles(t.Context(), host, hostd, nil, strings.TrimPrefix(health.URL, "http://"), false)
	if err == nil || !strings.Contains(err.Error(), "power policy unavailable") {
		t.Fatal("power policy failure was hidden or replaced", err)
	}
	if !runner.hostdStarted {
		t.Fatal("independent power policy prevented terminal runtime from starting")
	}
}

type preparedPowerRunner struct {
	paused, powerReady, powerStopped, hostdStopped bool
	powerErr                                       error
}

func (r *preparedPowerRunner) Run(_ context.Context, _ string, args ...string) error {
	if len(args) < 2 {
		return nil
	}
	unit := args[len(args)-1]
	if unit == "power.service" && (args[0] == "restart" || args[0] == "enable" && args[1] == "--now") {
		if r.paused {
			return errors.New("power startup entered terminal cutover")
		}
		if r.powerErr != nil {
			return r.powerErr
		}
		r.powerReady = true
	}
	if args[0] == "stop" {
		if unit == "power.service" {
			r.powerStopped = true
		}
		if unit == "hostd.service" {
			r.hostdStopped = true
		}
	}
	return nil
}
func (r *preparedPowerRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	return "", r.Run(ctx, name, args...)
}

func TestPowerReplacementCompletesBeforeTerminalAdmissionPauses(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "healthy"
		if fail {
			name = "failed"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			pin := filepath.Join(root, "pinned-pb")
			if err := os.WriteFile(pin, []byte("fixture executable"), 0755); err != nil {
				t.Fatal(err)
			}
			runner := &preparedPowerRunner{}
			if fail {
				runner.powerErr = errors.New("power startup failed")
			}
			makeRole := func(kind, unit, image string) *service.Installer {
				t.Helper()
				i, err := service.NewPending(service.Config{Platform: "linux", Kind: kind, Instance: "u1001", ConfigRoot: root, Executable: image, User: "tester", Group: "tester", Arguments: []string{"daemon"}, Controller: service.SystemdController{Runner: runner, Unit: unit}})
				if err != nil {
					t.Fatal(err)
				}
				return i
			}
			power := makeRole(service.HostKind, "power.service", pin)
			hostd := makeRole(service.HostdKind, "hostd.service", filepath.Join(root, "pb"))
			err := prepareUnixRuntimeCutover(t.Context(), power, hostd, nil, func(context.Context) error {
				if !runner.powerReady {
					return errors.New("power service was not prepared before admission")
				}
				runner.paused = true
				return nil
			})
			if fail {
				if !errors.Is(err, runner.powerErr) || runner.paused || runner.hostdStopped {
					t.Fatalf("power failure interrupted serving terminal: err=%v paused=%v stopped=%v", err, runner.paused, runner.hostdStopped)
				}
				return
			}
			if err != nil || !runner.paused || !runner.hostdStopped || runner.powerStopped {
				t.Fatalf("independent power replacement crossed terminal cutover: err=%v paused=%v hostdStopped=%v powerStopped=%v", err, runner.paused, runner.hostdStopped, runner.powerStopped)
			}
		})
	}
}

func TestPowerServiceUsesTheImmutableCandidatePin(t *testing.T) {
	root := t.TempDir()
	paths := installPaths{worker: filepath.Join(root, "pb"), workerRollback: filepath.Join(root, "releases", "pb.rollback")}
	request := Request{Platform: runtime.GOOS, UID: 1001, GID: 1001, HelperListenAddress: "127.0.0.1:1234", Source: installsource.Source{SHA256: strings.Repeat("a", 64)}}
	installer, err := hostInstallerWithMissing(request, paths, true)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "releases", "pins", request.Source.SHA256, "pb")
	if installer.ExecutablePath() != want {
		t.Fatalf("power service depends on mutable worker slot: %s", installer.ExecutablePath())
	}
}
