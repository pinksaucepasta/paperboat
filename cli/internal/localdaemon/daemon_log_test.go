//go:build darwin || linux

package localdaemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDaemonLogMovesAsideAtItsLimitAndWritingContinuesInAFreshFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paperboatd.log")
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	fd := int(out.Fd())
	if _, err := out.WriteString("old\n"); err != nil {
		t.Fatal(err)
	}
	if err := rotateDaemonLog(path, 100, fd); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("rotated below the limit: %v", err)
	}
	if _, err := out.WriteString(strings.Repeat("x", 200) + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := rotateDaemonLog(path, 100, fd); err != nil {
		t.Fatal(err)
	}
	if _, err := out.WriteString("new\n"); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(path); string(body) != "new\n" {
		t.Fatalf("fresh log holds %q", body)
	}
	if body, _ := os.ReadFile(path + ".1"); !strings.HasPrefix(string(body), "old\n") {
		t.Fatalf("rotated log holds %q", body)
	}
	// A daemon run by hand writes to its terminal, not the log: nothing moves.
	other, err := os.CreateTemp(t.TempDir(), "tty")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := os.WriteFile(path, []byte(strings.Repeat("y", 300)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rotateDaemonLog(path, 100, int(other.Fd())); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(path + ".1"); !strings.HasPrefix(string(body), "old\n") {
		t.Fatal("rotated a log the fd does not write to")
	}
}
