package daemoncmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

// A user daemon started before `pb pair` waits for enrollment. Pairing on a headless computer then rewrites
// config.json to use the protected file credential store; the daemon kept the configuration it read at start
// (OS keyring), so every control-plane call failed with a credential error and the daemon sat at
// "degraded: Control plane is unavailable" until restarted (laneh-relC, pbvm2, 2026-10-22).
func TestDaemonRunsWithTheConfigurationPairingWrote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	started, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := started.Save(); err != nil {
		t.Fatal(err)
	}
	enrolled := false
	machineID := func() (string, error) {
		if !enrolled {
			return "", os.ErrNotExist
		}
		return "machine_01", nil
	}
	waits := 0
	pair := func(*config.Config) error {
		waits++
		paired, err := config.Load(path)
		if err != nil {
			return err
		}
		paired.Auth.AllowFileFallback = true
		enrolled = true
		return paired.Save()
	}
	cfg, id, err := enrolledConfig(func() (*config.Config, error) { return config.Load(path) }, machineID, pair)
	if err != nil || id != "machine_01" || waits != 1 {
		t.Fatalf("id=%q waits=%d err=%v", id, waits, err)
	}
	if !cfg.Auth.AllowFileFallback {
		t.Fatal("daemon kept the configuration it read before pairing; its credential store is the wrong one")
	}
}
