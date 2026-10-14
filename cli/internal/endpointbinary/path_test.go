package endpointbinary

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCurrentExecutableIsUsedWithoutPATHFallback(t *testing.T) {
	root := t.TempDir()
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	cli := filepath.Join(root, "pb"+suffix)
	if err := os.WriteFile(cli, nil, 0755); err != nil {
		t.Fatal(err)
	}
	if got, err := Daemon(cli); err != nil || got != cli {
		t.Fatalf("daemon = %q, %v", got, err)
	}
	if got, err := CLI(cli); err != nil || got != cli {
		t.Fatalf("CLI = %q, %v", got, err)
	}
	if _, err := Daemon("pb"); err == nil {
		t.Fatal("relative executable accepted")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(cli, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Daemon(cli); err == nil {
			t.Fatal("non-executable daemon accepted")
		}
	}
}

func TestDaemonRemovalPathDoesNotRequireDaemonArtifact(t *testing.T) {
	root := t.TempDir()
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	cli := filepath.Join(root, "pb"+suffix)
	if err := os.WriteFile(cli, nil, 0755); err != nil {
		t.Fatal(err)
	}
	want := cli
	if got, err := DaemonPathForRemoval(cli); err != nil || got != want {
		t.Fatalf("removal path=%q err=%v", got, err)
	}
	if _, err := DaemonPathForRemoval("pb"); err == nil {
		t.Fatal("relative removal anchor accepted")
	}
}

func TestMissingExecutableRetainsNativeCauseWithoutPrivatePathAndRecovers(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "PRIVATE_EXECUTABLE_PATH")
	for _, resolve := range []func(string) (string, error){CLI, Daemon, DaemonPathForRemoval} {
		value, err := resolve(path)
		var native *os.PathError
		if value != "" || !errors.Is(err, os.ErrNotExist) || !errors.As(err, &native) {
			t.Fatal("missing executable lost original filesystem cause")
		}
		if strings.Contains(err.Error(), "PRIVATE_EXECUTABLE_PATH") || strings.Contains(err.Error(), root) {
			t.Fatal("missing executable exposed private path")
		}
	}
	if err := os.WriteFile(path, []byte("fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	if value, err := CLI(path); err != nil || value != path {
		t.Fatal("fresh executable resolution did not recover")
	}
}
