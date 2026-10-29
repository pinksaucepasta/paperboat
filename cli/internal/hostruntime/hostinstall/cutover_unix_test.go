//go:build linux || darwin

package hostinstall

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
