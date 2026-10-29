//go:build linux || darwin

package hostinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
