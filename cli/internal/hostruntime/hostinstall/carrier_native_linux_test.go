//go:build linux

package hostinstall

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
)

func TestCarrierProtectedCapabilityAndLifetimeLock(t *testing.T) {
	if input := os.Getenv("PAPERBOAT_TEST_CARRIER_CONFIG"); input != "" {
		var c CarrierConfig
		if err := json.Unmarshal([]byte(input), &c); err != nil {
			t.Fatal(err)
		}
		if os.Getenv("PAPERBOAT_TEST_CARRIER_FOREIGN") == "1" {
			if _, err := os.ReadFile(c.DispatchCapabilityFile); !errors.Is(err, os.ErrPermission) {
				t.Fatal("shared-primary-group account read raw capability", err)
			}
			if _, err := ReadCarrierCapability(context.Background(), c); err == nil {
				t.Fatal("foreign account read capability")
			}
			if lock, err := LockCarrier(c); err == nil {
				lock.Close()
				t.Fatal("foreign account acquired carrier lock")
			}
			return
		}
		if token, err := ReadCarrierCapability(context.Background(), c); err != nil || len(token) != 32 {
			t.Fatal("enrolled capability unavailable", err)
		}
		first, err := LockCarrier(c)
		if err != nil {
			t.Fatal(err)
		}
		if second, err := LockCarrier(c); err == nil {
			second.Close()
			t.Fatal("second carrier acquired account lock")
		}
		if err := os.Remove(c.LockPath); !errors.Is(err, os.ErrPermission) {
			t.Fatal("enrolled account could replace lifetime lock", err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		next, err := LockCarrier(c)
		if err != nil {
			t.Fatal("closed carrier retained lock", err)
		}
		if err := next.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("requires protected native account fixture")
	}
	var entropy [4]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		t.Fatal(err)
	}
	uid := 1000000000 + int(binary.LittleEndian.Uint32(entropy[:])%1000000000)
	paths := platformPaths(uid)
	root := filepath.Dir(carrierConfigPath(paths.instance))
	var created []string
	for path := root; path != "/"; path = filepath.Dir(path) {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			created = append(created, path)
		} else if err != nil {
			t.Fatal(err)
		}
	}
	// Remove only fixture directories this test created, from child to parent.
	t.Cleanup(func() {
		for _, path := range created {
			if path == root {
				for _, leaf := range []string{"config.json", "dispatch.cap", "owner.lock", "qualification.test"} {
					if err := os.Remove(filepath.Join(root, leaf)); err != nil && !errors.Is(err, os.ErrNotExist) {
						t.Error(err)
					}
				}
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Error(err)
			}
		}
	})
	for i := len(created) - 1; i >= 0; i-- {
		if err := os.Mkdir(created[i], 0755); err != nil {
			t.Fatal(err)
		}
	}
	c := CarrierConfig{Schema: carrierSchema, Instance: paths.instance, UID: uid, GID: uid, User: "fixture", Group: "fixture", RuntimeRoot: "/var/lib", Source: installsource.Source{Version: "fixture", Platform: runtime.GOOS, Architecture: runtime.GOARCH, SHA256: strings.Repeat("a", 64), Length: 1, Distribution: installsource.Custom}, WorkerAddress: "127.0.0.1:2026", HostdEndpoint: paths.hostdSocket, ControlEndpoint: paths.hostdSocket + ".carrier", HostdTokenFile: paths.hostdToken, DispatchCapabilityFile: filepath.Join(root, "dispatch.cap"), LockPath: filepath.Join(root, "owner.lock")}
	c.Executable = filepath.Join(filepath.Dir(paths.workerRollback), "pins", c.Source.SHA256, "pb")
	c.Environment = map[string]string{"PAPERBOAT_RUNTIME_STATE_ROOT": c.RuntimeRoot, "PAPERBOAT_RUNTIME_LISTEN_ADDRESS": c.WorkerAddress, "PAPERBOAT_MACHINE_ID": "fixture"}
	capability := make([]byte, 32)
	if _, err := rand.Read(capability); err != nil {
		t.Fatal(err)
	}
	if err := publishCarrierFiles(c, carrierConfigPath(c.Instance), capability); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sourceFile, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "qualification.test")
	targetFile, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		sourceFile.Close()
		t.Fatal(err)
	}
	_, copyErr := io.Copy(targetFile, sourceFile)
	if err = errors.Join(copyErr, targetFile.Close(), sourceFile.Close()); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(target, "-test.run=^TestCarrierProtectedCapabilityAndLifetimeLock$")
	child.Env = append(os.Environ(), "PAPERBOAT_TEST_CARRIER_CONFIG="+string(body))
	child.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(uid)}}
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("enrolled custody failed: %v: %s", err, output)
	}
	foreign := exec.Command(target, "-test.run=^TestCarrierProtectedCapabilityAndLifetimeLock$")
	foreign.Env = append(os.Environ(), "PAPERBOAT_TEST_CARRIER_CONFIG="+string(body), "PAPERBOAT_TEST_CARRIER_FOREIGN=1")
	foreign.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid + 1), Gid: uint32(uid)}}
	if output, err := foreign.CombinedOutput(); err != nil {
		t.Fatalf("shared-group isolation failed: %v: %s", err, output)
	}
	for _, mode := range []os.FileMode{0644, 0660} {
		if err := os.Chmod(c.DispatchCapabilityFile, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadCarrierCapability(context.Background(), c); err == nil {
			t.Fatal("unsafe capability custody admitted mode", strconv.FormatUint(uint64(mode), 8))
		}
	}
	if err := os.Chmod(c.DispatchCapabilityFile, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.DispatchCapabilityFile, capability[:31], 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCarrierCapability(context.Background(), c); err == nil {
		t.Fatal("short capability admitted")
	}
	if err := os.Remove(c.DispatchCapabilityFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(c.LockPath, c.DispatchCapabilityFile); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCarrierCapability(context.Background(), c); err == nil {
		t.Fatal("symlink capability admitted")
	}
}
