//go:build linux || darwin

package localdaemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hostservice "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"howett.net/plist"
)

type stoppedLaunchdRunner struct {
	registered bool
	calls      []string
}

func (r *stoppedLaunchdRunner) Run(_ context.Context, tool string, args ...string) error {
	r.calls = append(r.calls, tool+" "+strings.Join(args, " "))
	switch args[0] {
	case "bootstrap":
		r.registered = true
	case "bootout":
		r.registered = false
	case "print", "kickstart":
		if !r.registered {
			return errors.New("launchctl: service not found")
		}
	}
	return nil
}
func (r *stoppedLaunchdRunner) Output(ctx context.Context, tool string, args ...string) (string, error) {
	err := r.Run(ctx, tool, args...)
	if err != nil {
		return "", err
	}
	return "job = {\nstate = running\npid = 123\n}", nil
}

func TestDarwinStoppedDeclarationCanStartWithoutReinstall(t *testing.T) {
	runner := &stoppedLaunchdRunner{}
	config := serviceConfig{Platform: "darwin", Home: t.TempDir(), Executable: serviceExecutable(t), Username: "test", Group: "staff", UID: os.Geteuid(), Runner: runner}
	installer, err := newServiceInstaller(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = installer.Install(t.Context()); err != nil {
		t.Fatal(err)
	}
	definition, _ := serviceDefinition(config)
	controller := definition.Controller.(hostservice.NativeLifecycleController)
	if err = controller.Stop(t.Context(), installer.DefinitionPath()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(installer.DefinitionPath())
	if err != nil {
		t.Fatal(err)
	}
	if installed, err := darwinServiceInstalled(installer.DefinitionPath(), config.Executable, config.UID); err != nil || !installed {
		t.Fatalf("stopped installation: %v %v", installed, err)
	}
	runner.calls = nil
	if err = startUserService(t.Context(), controller, installer.DefinitionPath(), "darwin", config.Executable, config.UID); err != nil {
		t.Fatal(err)
	}
	if !runner.registered || !strings.Contains(strings.Join(runner.calls, "\n"), "launchctl bootstrap gui/") {
		t.Fatalf("did not bootstrap: %v", runner.calls)
	}
	after, _ := os.ReadFile(installer.DefinitionPath())
	if string(after) != string(before) {
		t.Fatal("start rewrote declaration")
	}
	var changed map[string]any
	if _, err = plist.Unmarshal(before, &changed); err != nil {
		t.Fatal(err)
	}
	changed["ProgramArguments"] = []string{filepath.Join(config.Home, "foreign"), "daemon"}
	body, err := plist.Marshal(changed, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(installer.DefinitionPath(), body, 0600); err != nil {
		t.Fatal(err)
	}
	runner.calls = nil
	if err = startUserService(t.Context(), controller, installer.DefinitionPath(), "darwin", config.Executable, config.UID); err == nil {
		t.Fatal("foreign executable accepted")
	}
	if len(runner.calls) != 0 {
		t.Fatal("controller called before declaration validation")
	}
}

func TestDarwinInstalledDeclarationRejectsUnsafeFiles(t *testing.T) {
	config := serviceConfig{Platform: "darwin", Home: t.TempDir(), Executable: serviceExecutable(t), Username: "test", Group: "staff", UID: os.Geteuid(), Runner: &stoppedLaunchdRunner{}}
	installer, err := newServiceInstaller(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = installer.Install(t.Context()); err != nil {
		t.Fatal(err)
	}
	path := installer.DefinitionPath()
	if _, err = darwinServiceInstalled(path, config.Executable, config.UID+1); err == nil {
		t.Fatal("foreign owner accepted")
	}
	if err = os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err = darwinServiceInstalled(path, config.Executable, config.UID); err == nil {
		t.Fatal("writable plist accepted")
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(config.Home, "alias.plist")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err = darwinServiceInstalled(link, config.Executable, config.UID); err == nil {
		t.Fatal("symlink plist accepted")
	}
	if installed, err := darwinServiceInstalled(filepath.Join(config.Home, "absent"), config.Executable, config.UID); err != nil || installed {
		t.Fatalf("missing plist=%t %v", installed, err)
	}
}
