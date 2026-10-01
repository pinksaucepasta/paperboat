package daemonrpc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDarwinSocketNamespaceIndependentOfTemporaryEnvironment(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "pbdrpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("PAPERBOAT_DAEMON_SOCK", "")
	expected := filepath.Join(home, "Library", "Application Support", "Paperboat", "state", "run", "daemon.sock")
	for _, temp := range []string{"", "/tmp", "/private/var/folders/gui/T/", "/missing", "relative"} {
		t.Run(temp, func(t *testing.T) {
			t.Setenv("TMPDIR", temp)
			t.Setenv("XDG_RUNTIME_DIR", temp)
			if got := DefaultSocketAddress(); got != expected {
				t.Fatalf("endpoint=%q, want %q", got, expected)
			}
		})
	}
	for _, dir := range []string{filepath.Dir(expected), filepath.Dir(filepath.Dir(expected))} {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			t.Fatalf("unprotected namespace %q: %v, %v", dir, info, err)
		}
	}
	if err := os.Chmod(filepath.Dir(expected), 0755); err != nil {
		t.Fatal(err)
	}
	if got := DefaultSocketAddress(); got != "" {
		t.Fatalf("unsafe parent accepted or fallback selected: %q", got)
	}
}
