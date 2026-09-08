//go:build darwin || linux

package localdaemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hostservice "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

type serviceRunner struct{ calls []string }

func (r *serviceRunner) Run(_ context.Context, name string, arguments ...string) error {
	r.calls = append(r.calls, strings.Join(append([]string{name}, arguments...), " "))
	return nil
}

func serviceExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pb")
	if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInstallServiceUsesUserManagerAndExactArguments(t *testing.T) {
	for _, platform := range []string{"darwin", "linux"} {
		t.Run(platform, func(t *testing.T) {
			home := t.TempDir()
			configPath := filepath.Join(home, ".config", "paperboat", "config.json")
			runner := &serviceRunner{}
			err := installService(context.Background(), serviceConfig{
				Platform: platform, Home: home, Executable: serviceExecutable(t), ConfigPath: configPath,
				ServerURL: "https://api.paperboat.test", Username: "test", Group: "staff", UID: 501,
				Environment: map[string]string{"HOME": home}, Runner: runner,
			})
			if err != nil {
				t.Fatal(err)
			}
			definition := filepath.Join(home, ".config", "systemd", "user", "paperboatd.service")
			if platform == "darwin" {
				definition = filepath.Join(home, "Library", "LaunchAgents", hostservice.DaemonLabel+".plist")
			}
			body, err := os.ReadFile(definition)
			if err != nil {
				t.Fatal(err)
			}
			text := string(body)
			for _, expected := range []string{"daemon", configPath, "https://api.paperboat.test", home} {
				if !strings.Contains(text, expected) {
					t.Fatalf("definition missing %q:\n%s", expected, text)
				}
			}
			joined := strings.Join(runner.calls, "\n")
			if platform == "linux" {
				for _, expected := range []string{"systemctl --user daemon-reload", "systemctl --user enable --now paperboatd.service", "systemctl --user is-active --quiet paperboatd.service"} {
					if !strings.Contains(joined, expected) {
						t.Fatalf("calls missing %q:\n%s", expected, joined)
					}
				}
			} else {
				for _, expected := range []string{"launchctl bootstrap gui/501", "launchctl kickstart -k gui/501/" + hostservice.DaemonLabel, "launchctl print gui/501/" + hostservice.DaemonLabel} {
					if !strings.Contains(joined, expected) {
						t.Fatalf("calls missing %q:\n%s", expected, joined)
					}
				}
			}
		})
	}
}

func TestInstallServiceRejectsRelativeConfigAndUnsupportedPlatform(t *testing.T) {
	base := serviceConfig{Platform: "linux", Home: t.TempDir(), Executable: serviceExecutable(t), ConfigPath: "relative", Username: "test", Group: "staff", UID: 501, Environment: map[string]string{"HOME": "/tmp"}, Runner: &serviceRunner{}}
	if err := installService(context.Background(), base); err == nil {
		t.Fatal("relative config path was accepted")
	}
	base.ConfigPath = ""
	base.Platform = "windows"
	if err := installService(context.Background(), base); err != hostservice.ErrUnsupportedPlatform {
		t.Fatalf("unsupported platform err=%v", err)
	}
}

func TestRemoveServiceStopsManagerBeforeDeletingDefinition(t *testing.T) {
	for _, platform := range []string{"darwin", "linux"} {
		t.Run(platform, func(t *testing.T) {
			home := t.TempDir()
			runner := &serviceRunner{}
			config := serviceConfig{Platform: platform, Home: home, Executable: serviceExecutable(t), Username: "test", Group: "staff", UID: 501, Environment: map[string]string{"HOME": home}, Runner: runner}
			installer, err := newServiceInstaller(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := installer.Install(context.Background()); err != nil {
				t.Fatal(err)
			}
			runner.calls = nil
			if err := installer.Uninstall(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(installer.DefinitionPath()); !os.IsNotExist(err) {
				t.Fatalf("definition remains: %v", err)
			}
			joined := strings.Join(runner.calls, "\n")
			if platform == "linux" && !strings.Contains(joined, "systemctl --user disable --now paperboatd.service") {
				t.Fatalf("linux calls=%s", joined)
			}
			if platform == "darwin" && !strings.Contains(joined, "launchctl bootout gui/501/"+hostservice.DaemonLabel) {
				t.Fatalf("darwin calls=%s", joined)
			}
		})
	}
}

func TestRemoveServiceWorksWithMissingDaemonArtifact(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			home := t.TempDir()
			definition := filepath.Join(home, ".config", "systemd", "user", "paperboatd.service")
			if platform == "darwin" {
				definition = filepath.Join(home, "Library", "LaunchAgents", hostservice.DaemonLabel+".plist")
			}
			if err := os.MkdirAll(filepath.Dir(definition), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(definition, []byte("existing service declaration"), 0600); err != nil {
				t.Fatal(err)
			}
			runner := &serviceRunner{}
			cfg := serviceConfig{Platform: platform, Home: home, Executable: filepath.Join(home, "pb"), Username: "test", Group: "staff", UID: 501, Runner: runner}
			if _, err := newServiceInstaller(cfg); err == nil {
				t.Fatal("missing daemon accepted for installation")
			}
			if err := removeService(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(definition); !os.IsNotExist(err) {
				t.Fatalf("definition remains: %v", err)
			}
			calls := strings.Join(runner.calls, "\n")
			if strings.Contains(calls, "enable --now") || strings.Contains(calls, "bootstrap") || strings.Contains(calls, "kickstart") {
				t.Fatalf("removal attempted startup: %s", calls)
			}
			if platform == "linux" && !strings.Contains(calls, "disable --now paperboatd.service") {
				t.Fatalf("missing service stop: %s", calls)
			}
			if platform == "darwin" && !strings.Contains(calls, "bootout gui/501/"+hostservice.DaemonLabel) {
				t.Fatalf("missing service stop: %s", calls)
			}
			count := len(runner.calls)
			if err := removeService(context.Background(), cfg); err != nil || len(runner.calls) != count {
				t.Fatalf("absent definition removal touched manager: %v", err)
			}
		})
	}
}
