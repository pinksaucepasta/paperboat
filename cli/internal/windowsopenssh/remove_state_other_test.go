//go:build !windows

package windowsopenssh

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type unusedRunner struct{}

func (unusedRunner) Run(context.Context, string, ...string) ([]byte, error) { return nil, nil }

// Uninstall removes the SSH access state even when the service could not be removed (here the
// non-Windows stub always fails): a leftover users\<instance>\ssh made the next install refuse its
// instance root (Windows test machine, second Windows user, 2026-11-16).
func TestRemovePaperboatStateRemovesAccessStateWhenTheServiceStepFails(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ssh")
	for _, f := range []string{"sshd_config", "hostkeys/ssh_host_ed25519_key", "hostkeys/ssh_host_ed25519_key.pub", "authorized_keys/paperboat", "authorized_keys/revoked.krl", "logs/sshd.log"} {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config := Config{Platform: "windows", Runner: unusedRunner{}, InstallRoot: t.TempDir(), StateRoot: root, ApprovedVersion: "9.8.1.0", ExpectedPublisher: "Microsoft", Port: 22}
	if err := RemovePaperboatState(context.Background(), config); err == nil {
		t.Fatal("the failed service step was not reported")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("ssh state left behind: %v", err)
	}
}
