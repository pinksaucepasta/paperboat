//go:build darwin || linux

package localdaemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/config"
)

// The service gets PAPERBOAT_CONFIG so the daemon resolves the same credential directory as the command
// that installed it (and its keychain check).
func TestCurrentUserServiceConfigKeepsConfigPath(t *testing.T) {
	t.Setenv(config.EnvConfigPath, "/srv/pb/config.json")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	service, err := currentUserServiceConfig(executable)
	if err != nil {
		t.Skipf("service config not resolvable for the test binary: %v", err)
	}
	if service.Environment[config.EnvConfigPath] != "/srv/pb/config.json" {
		t.Fatalf("service environment = %v", service.Environment)
	}
	t.Chdir(t.TempDir())
	t.Setenv(config.EnvConfigPath, "pb.json ")
	if service, err = currentUserServiceConfig(executable); err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.Abs("pb.json "); service.Environment[config.EnvConfigPath] != want {
		t.Fatalf("relative config = %q, want %q", service.Environment[config.EnvConfigPath], want)
	}
}
