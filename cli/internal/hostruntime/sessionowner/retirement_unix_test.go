//go:build linux || darwin

package sessionowner

import (
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestRetiredEndpointCleanupPreservesArchiveAndOtherGeneration(t *testing.T) {
	root := shorttemp.Dir(t)
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(root, "retired")
	other := filepath.Join(root, "active")
	archive := filepath.Join(root, "archive")
	for _, path := range []string{run, other, archive} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	endpoint := filepath.Join(run, "control.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: endpoint, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	d := Descriptor{Endpoint: endpoint, UID: os.Getuid(), RuntimeRoot: archive}
	if err := removeRetiredEndpoint(d); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(run); !os.IsNotExist(err) {
		t.Fatal("retired run directory remains")
	}
	for _, path := range []string{other, archive} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("cleanup removed another generation or archive")
		}
	}
	if err := removeRetiredEndpoint(d); err != nil {
		t.Fatal("cleanup retry failed")
	}
}

func TestRetiredEndpointCleanupRejectsUnsafeAndUnknownContent(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "file", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			root := shorttemp.Dir(t)
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			run := filepath.Join(root, "retired")
			if err := os.Mkdir(run, 0700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "preserved")
			if err := os.WriteFile(target, []byte("preserved"), 0600); err != nil {
				t.Fatal(err)
			}
			endpoint := filepath.Join(run, "control.sock")
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(target, endpoint)
			case "hardlink":
				err = os.Link(target, endpoint)
			case "file":
				err = os.WriteFile(endpoint, []byte("unexpected"), 0600)
			case "unknown":
				err = os.WriteFile(filepath.Join(run, "unknown"), []byte("unexpected"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := removeRetiredEndpoint(Descriptor{Endpoint: endpoint, UID: os.Getuid(), RuntimeRoot: filepath.Join(root, "store")}); err == nil {
				t.Fatal("unsafe or nonempty directory accepted")
			}
			if body, err := os.ReadFile(target); err != nil || string(body) != "preserved" {
				t.Fatal("foreign target changed")
			}
			if _, err := os.Stat(run); err != nil {
				t.Fatal("unsafe directory removed")
			}
		})
	}
}
