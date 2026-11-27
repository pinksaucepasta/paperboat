//go:build darwin || linux

package hostruntimecmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/config"
)

// On Linux and macOS a fresh pb install could not tell "no server anywhere" and went on to the install lock
// and a sudo prompt; a computer with no saved configuration and no installed service has a definite none.
func TestInheritedInstallServerOnAFreshComputer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv(config.EnvConfigPath, filepath.Join(home, "config", "paperboat.json"))
	if server, known := InheritedInstallServer(); !known || server != "" {
		t.Fatalf("fresh computer: server %q known %t; want a definite none", server, known)
	}
	path := os.Getenv(config.EnvConfigPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"server_url":"https://api.example.test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if server, known := InheritedInstallServer(); !known || server != "https://api.example.test" {
		t.Fatalf("saved configuration: server %q known %t", server, known)
	}
}
