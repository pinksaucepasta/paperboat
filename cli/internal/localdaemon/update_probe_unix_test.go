//go:build darwin || linux

package localdaemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

func TestUpdateProbeReadsInstalledNamespace(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			home := t.TempDir()
			path := updateProbeTestDeclaration(t, home, platform)
			body := "[Service]\nEnvironment=\"HOME=/wrong\"\nEnvironment=\"TMPDIR=/private/task run\"\nEnvironment=\"XDG_STATE_HOME=/state/100%%/$$data\"\nEnvironment=\"SECRET=excluded\"\n"
			if platform == "darwin" {
				body = `<?xml version="1.0"?><plist version="1.0"><dict><key>EnvironmentVariables</key><dict><key>HOME</key><string>/wrong</string><key>TMPDIR</key><string>/private/task run</string><key>XDG_STATE_HOME</key><string>/state/100%/$data</string><key>SECRET</key><string>excluded</string></dict></dict></plist>`
			}
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := updateServiceEnvironment(platform, home, os.Geteuid())
			if err != nil {
				t.Fatal(err)
			}
			if got["HOME"] != home || got["TMPDIR"] != "/private/task run" || got["XDG_STATE_HOME"] != "/state/100%/$data" || len(got) != 3 {
				t.Fatalf("unexpected namespace: %#v", got)
			}
		})
	}
}

func TestUpdateServiceDefinitionPath(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "Users", "paperboat")
	if got, want := updateServiceDefinitionPath("darwin", home), filepath.Join(home, "Library", "LaunchAgents", service.DaemonLabel+".plist"); got != want {
		t.Fatalf("darwin definition path = %q, want %q", got, want)
	}
	if got, want := updateServiceDefinitionPath("linux", home), filepath.Join(home, ".config", "systemd", "user", "paperboatd.service"); got != want {
		t.Fatalf("linux definition path = %q, want %q", got, want)
	}
}

func TestUpdateProbeRejectsUnsafeDeclaration(t *testing.T) {
	for _, scenario := range []string{"symlink", "writable", "oversize", "fifo", "wrong-owner", "relative-namespace"} {
		t.Run(scenario, func(t *testing.T) {
			home := t.TempDir()
			path := updateProbeTestDeclaration(t, home, "linux")
			body := []byte("[Service]\nEnvironment=\"TMPDIR=/private/run\"\n")
			uid := os.Geteuid()
			switch scenario {
			case "symlink":
				target := filepath.Join(home, "target")
				if err := os.WriteFile(target, body, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			default:
				if scenario == "oversize" {
					body = []byte(strings.Repeat("x", (64<<10)+1))
				}
				if scenario == "relative-namespace" {
					body = []byte("[Service]\nEnvironment=\"TMPDIR=relative\"\n")
				}
				if scenario == "wrong-owner" {
					uid++
				}
				if err := os.WriteFile(path, body, 0600); err != nil {
					t.Fatal(err)
				}
				if scenario == "writable" {
					if err := os.Chmod(path, 0666); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := updateServiceEnvironment("linux", home, uid); err == nil {
				t.Fatal("accepted unsafe declaration")
			}
		})
	}
}

func TestUpdateProbeMissingDeclaration(t *testing.T) {
	_, err := updateServiceEnvironment("linux", t.TempDir(), os.Geteuid())
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing declaration: %v", err)
	}
}

func updateProbeTestDeclaration(t *testing.T, home, platform string) string {
	t.Helper()
	path := filepath.Join(home, ".config/systemd/user/paperboatd.service")
	if platform == "darwin" {
		path = filepath.Join(home, "Library/LaunchAgents/com.pinksaucepasta.paperboatd.plist")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
