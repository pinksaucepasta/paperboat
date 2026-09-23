package endpointbinary

import (
	"os"
	"path/filepath"
	"runtime"
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
