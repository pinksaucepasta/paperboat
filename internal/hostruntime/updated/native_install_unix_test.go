//go:build darwin || linux

package updated

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"strings"
)

func TestUnixNativeInstallRetiresIdleStateAndRestoresOnRollback(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected native installer state requires root")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "transaction.json")
	j := updateflow.Journal{Schema: updateflow.SchemaV1, TransactionID: "native-install-fixture", Stage: updateflow.StageIdle, ActiveVersion: "2026.09.19.0", BootID: "hostd", StageUpdatedAt: time.Now().UTC(), ActiveDigest: strings.Repeat("a", 64), ActiveLength: 1, ActiveHostdAPIMin: 1, ActiveHostdAPIMax: 1, ActiveRuntimeAPIMin: 1, ActiveRuntimeAPIMax: 1}
	if err := updateflow.Write(path, j, 0, 0); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tuf := filepath.Join(root, "trust-proof")
	if err = os.WriteFile(tuf, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := beginNativeInstallFixture(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old transaction retained: %v", err)
	}
	lock.Close()
	if pending, err := nativeInstallPending(root); err != nil || !pending {
		t.Fatal("install not fenced", err)
	}
	if err = RollbackUnixNativeInstall(root); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("prior transaction not restored exactly", err)
	}
	lock, err = beginNativeInstallFixture(root)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	if err = CommitUnixNativeInstall(root); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("committed installation retained old baseline")
	}
	if data, err := os.ReadFile(tuf); err != nil || string(data) != "unchanged" {
		t.Fatal("trust state changed", err)
	}
}

func TestUnixNativeInstallRejectsPendingActivationWithoutMutation(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected native installer state requires root")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	h := &unixActivationHandoff{Schema: unixHandoffSchema, Previous: workerupdate.Release{Version: "2026.09.19.0", SHA256: strings.Repeat("a", 64), Length: 1}, Candidate: "2026.09.19.1"}
	if err := writeUnixHandoff(root, h); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(unixHandoffPath(root))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := beginNativeInstallFixture(root)
	if lock != nil {
		lock.Close()
	}
	if !errors.Is(err, ErrActivationPending) {
		t.Fatal("pending update accepted", err)
	}
	after, err := os.ReadFile(unixHandoffPath(root))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("pending recovery changed", err)
	}
	if pending, err := nativeInstallPending(root); err != nil || pending {
		t.Fatal("failed installation left marker", err)
	}
}

func beginNativeInstallFixture(root string) (io.Closer, error) {
	lock, err := LockUnixNativeInstall(root)
	if err != nil {
		return nil, err
	}
	if err = PrepareUnixNativeInstall(root); err != nil {
		lock.Close()
		return nil, err
	}
	return lock, nil
}
