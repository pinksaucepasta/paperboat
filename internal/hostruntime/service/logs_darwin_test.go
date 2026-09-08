//go:build darwin

package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestNativeServiceLogOwnershipAndUnsafeFiles(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root for enrolled-user ownership qualification")
	}
	root, err := os.MkdirTemp("/private/tmp", "paperboat-service-log-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	const uid = 501
	const gid = 20
	path := filepath.Join(root, "hostd.log")
	if err := os.WriteFile(path, []byte("retained diagnostic\n"), 0600); err != nil {
		t.Fatal(err)
	}
	writable := func() bool {
		return exec.Command("/usr/bin/sudo", "-n", "-u", "#501", "/bin/test", "-w", path).Run() == nil
	}
	if writable() {
		t.Fatal("root-only fixture unexpectedly writable by enrolled user")
	}
	if err := prepareServiceLog(path, uid, gid); err != nil {
		t.Fatal(err)
	}
	if !writable() {
		t.Fatal("prepared log remains unwritable by enrolled user")
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "retained diagnostic\n" {
		t.Fatal("existing diagnostics changed")
	}
	var st unix.Stat_t
	if err := unix.Stat(root, &st); err != nil || st.Uid != 0 || st.Mode&0777 != 0755 {
		t.Fatal("trusted parent changed")
	}
	for _, kind := range []string{"symlink", "hardlink", "foreign", "directory"} {
		t.Run(kind, func(t *testing.T) {
			target := filepath.Join(root, kind)
			switch kind {
			case "symlink":
				err = os.Symlink(path, target)
			case "hardlink":
				err = os.Link(path, target)
			case "directory":
				err = os.Mkdir(target, 0700)
			case "foreign":
				err = os.WriteFile(target, []byte("foreign"), 0600)
				if err == nil {
					err = os.Chown(target, 502, 20)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			var before unix.Stat_t
			if err := unix.Lstat(target, &before); err != nil {
				t.Fatal(err)
			}
			if err := prepareServiceLog(target, uid, gid); err == nil {
				t.Fatal("accepted unsafe log")
			}
			var after unix.Stat_t
			if err := unix.Lstat(target, &after); err != nil {
				t.Fatal(err)
			}
			if before.Uid != after.Uid || before.Mode != after.Mode || before.Size != after.Size {
				t.Fatal("unsafe object changed")
			}
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
		})
	}
}
