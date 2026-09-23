//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

func TestUnixManualRefresherConstruction(t *testing.T) {
	layout, err := service.UserLayout(runtime.GOOS, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newUnixManualRefresher(layout.Binary, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		binary   string
		uid, gid int
	}{{"/tmp/pb", 1000, 1000}, {layout.Binary, -1, 1000}, {layout.Binary, 1000, -1}, {layout.Binary, 1000, 0}} {
		if _, err := newUnixManualRefresher(tc.binary, tc.uid, tc.gid); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
}

func TestUnixManualRefreshCommandIdentity(t *testing.T) {
	owner := hostinstall.Request{UID: 1234, GID: 5678, Home: "/home/enrolled", User: "enrolled"}
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			cmd, err := unixManualRefreshCommand(context.Background(), "/protected/bin/pb", platform, owner)
			if err != nil {
				t.Fatal(err)
			}
			directory, home, username := "/home/enrolled/.local/share/man", owner.Home, owner.User
			uid, gid := uint32(owner.UID), uint32(owner.GID)
			if platform == "darwin" {
				directory, home, username, uid, gid = "/usr/local/share/man", "/var/root", "root", 0, 0
			}
			want := []string{"/protected/bin/pb", "--no-customization", "__man-pages", "--directory", directory}
			if !reflect.DeepEqual(cmd.Args, want) {
				t.Fatalf("args=%q", cmd.Args)
			}
			cred := cmd.SysProcAttr.Credential
			if cred.Uid != uid || cred.Gid != gid || !reflect.DeepEqual(cred.Groups, []uint32{gid}) || !cmd.SysProcAttr.Setpgid {
				t.Fatalf("credential=%+v", cred)
			}
			env := []string{"HOME=" + home, "USER=" + username, "LOGNAME=" + username, "PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"}
			if !reflect.DeepEqual(cmd.Env, env) {
				t.Fatalf("environment=%q", cmd.Env)
			}
			if cmd.Stdout != io.Discard || cmd.Stderr != io.Discard || cmd.Cancel == nil || cmd.WaitDelay != time.Second {
				t.Fatal("missing bounded subprocess controls")
			}
		})
	}
}

func TestUnixManualRefreshCancellationAndSafeError(t *testing.T) {
	owner := hostinstall.Request{UID: os.Getuid(), GID: os.Getgid(), Home: t.TempDir(), User: "test"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runUnixManualRefresh(ctx, "/secret-canary/missing", "linux", owner)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, errManualRefreshPending) || strings.Contains(err.Error(), "secret-canary") {
		t.Fatalf("error=%v", err)
	}
	ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err = runUnixManualRefresh(ctx, "/secret-canary/missing", "linux", owner)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parent deadline not preserved: %v", err)
	}
}

func TestSystemManualDirectoryRejectsUnsafeAncestor(t *testing.T) {
	// This must fail even when the target leaf itself does not exist; the shared
	// temporary ancestor is writable by other users and must never be traversed.
	root := t.TempDir()
	target := filepath.Join(root, "uncreated", "man1")
	if err := prepareSystemManualDirectory(target); err == nil {
		t.Fatal("accepted writable or user-owned ancestor")
	}
	if _, err := os.Lstat(filepath.Join(root, "uncreated")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created descendant under unsafe ancestor: %v", err)
	}
}
