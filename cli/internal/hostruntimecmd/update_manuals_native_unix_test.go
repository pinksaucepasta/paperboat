//go:build darwin || linux

package hostruntimecmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
)

// Opt-in because this exercises the real macOS system manual destination. The
// native acceptance owner installs a task package and restores the prior PKG.
func TestNativeDarwinManualRefreshRepair(t *testing.T) {
	binary := os.Getenv("PAPERBOAT_TEST_MANUAL_BINARY")
	if runtime.GOOS != "darwin" || binary == "" {
		t.Skip("native macOS manual fixture not selected")
	}
	if os.Geteuid() != 0 {
		t.Fatal("native manual fixture requires root")
	}
	section := "/usr/local/share/man/man1"
	page := filepath.Join(section, "pb.1")
	want, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(want, []byte(".\\\" Paperboat managed manual v1\n")) {
		t.Fatal("fixture package manuals must be installed first")
	}
	t.Cleanup(func() {
		_ = os.Remove(page)
		if err := os.WriteFile(page, want, 0644); err != nil {
			t.Error(err)
		}
	})
	if err := prepareSystemManualDirectory(section); err != nil {
		t.Fatal(err)
	}
	owner := hostinstall.Request{UID: 0, GID: 0, Home: "/var/root", User: "root"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	stale := filepath.Join(section, "pb-native-test-obsolete.1")
	foreign := filepath.Join(section, "pb-native-test-unowned.1")
	for path, data := range map[string][]byte{page: []byte("old manual"), stale: []byte(".\\\" Paperboat managed manual v1\nold command"), foreign: []byte("keep user manual")} {
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Remove(stale); _ = os.Remove(foreign) })
	if err := runUnixManualRefresh(ctx, binary, "darwin", owner); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(page)
	if !bytes.Equal(got, want) {
		t.Fatal("committed binary did not repair manual")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("obsolete owned manual retained", err)
	}
	if data, _ := os.ReadFile(foreign); string(data) != "keep user manual" {
		t.Fatal("unowned manual changed")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(page); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, page); err != nil {
		t.Fatal(err)
	}
	if err := runUnixManualRefresh(ctx, binary, "darwin", owner); !errors.Is(err, errManualRefreshPending) {
		t.Fatal("unsafe destination did not leave refresh pending", err)
	}
	if data, _ := os.ReadFile(outside); string(data) != "untouched" {
		t.Fatal("symlink target modified")
	}
	if err := os.Remove(page); err != nil {
		t.Fatal(err)
	}
	if err := runUnixManualRefresh(ctx, binary, "darwin", owner); err != nil {
		t.Fatal("retry failed", err)
	}
	got, _ = os.ReadFile(page)
	if !bytes.Equal(got, want) {
		t.Fatal("retry did not restore exact bundled page")
	}
}
