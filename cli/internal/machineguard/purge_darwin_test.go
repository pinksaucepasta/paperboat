//go:build darwin

package machineguard

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type darwinPurgeFixture struct {
	paths      darwinPurgePaths
	dsclRecord map[string]string
	calls      []string
}

func newDarwinPurgeFixture(t *testing.T) *darwinPurgeFixture {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("protected guard lifecycle fixture requires root")
	}
	root, err := os.MkdirTemp("/tmp", "paperboat-purge-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	fixture := &darwinPurgeFixture{
		paths: darwinPurgePaths{
			LaunchPath: filepath.Join(root, "LaunchDaemons", "io.paperboat.machineguard.plist"),
			HelperPath: filepath.Join(root, "PrivilegedHelperTools", "io.paperboat.machineguard"),
			StateDir:   filepath.Join(root, "Application Support", "Paperboat", "machineguard"),
			SocketDir:  filepath.Join(root, "Application Support", "Paperboat", "machineguard-control"),
			Anchor:     "com.apple/000.paperboat-machineguard",
		},
		dsclRecord: map[string]string{},
	}
	if err := os.MkdirAll(fixture.paths.SocketDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// run answers the launchctl, pfctl and dscl queries purge makes. Accounts are
// present only when listed in dsclRecord, with the RealName stored there.
func (f *darwinPurgeFixture) run(_ context.Context, name string, args ...string) ([]byte, error) {
	command := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, command)
	if name == "/usr/bin/dscl" && len(args) >= 3 && args[1] == "-read" {
		account := strings.TrimPrefix(args[2], "/Users/")
		realName, ok := f.dsclRecord[account]
		if !ok {
			return nil, errors.New("eDSRecordNotFound")
		}
		return []byte("RealName:\n " + realName + "\n"), nil
	}
	return nil, nil
}

func (f *darwinPurgeFixture) purge(ctx context.Context) error {
	return purgeDarwin(ctx, darwinPurge{
		Paths: f.paths,
		Run:   f.run,
		LocalNames: func(context.Context) error {
			return nil
		},
	})
}

func TestPurgeDarwinAfterRunningExecutableRemoved(t *testing.T) {
	if os.Getenv("PAPERBOAT_PURGE_EXECUTABLE_REMOVED_TEST") != "1" {
		f := newDarwinPurgeFixture(t)
		source, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		in, err := os.Open(source)
		if err != nil {
			t.Fatal(err)
		}
		defer in.Close()
		childPath := filepath.Join(filepath.Dir(f.paths.LaunchPath), "test-child")
		if err := os.MkdirAll(filepath.Dir(childPath), 0755); err != nil {
			t.Fatal(err)
		}
		out, err := os.OpenFile(childPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := io.Copy(out, in)
		if err := errors.Join(copyErr, out.Close()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, childPath, "-test.run=^TestPurgeDarwinAfterRunningExecutableRemoved$", "-test.count=1")
		command.Env = append(os.Environ(), "PAPERBOAT_PURGE_EXECUTABLE_REMOVED_TEST=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("unlinked executable purge: %v %s", err, output)
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(executable); err != nil {
		t.Fatal(err)
	}
	f := newDarwinPurgeFixture(t)
	if err := os.MkdirAll(f.paths.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{f.paths.LaunchPath, f.paths.HelperPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
	}
	declaration := darwinLaunchMarker + "\n" + uninstallDarwinArguments([]string{"daemon", "machine-guard", "run"})
	if err := os.WriteFile(f.paths.LaunchPath, []byte(declaration), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.paths.HelperPath, []byte("owned guard fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := f.purge(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{f.paths.StateDir, f.paths.LaunchPath, f.paths.HelperPath} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("guard resource retained after direct purge", err)
		}
	}
}

func TestPurgeDarwinPreservesResourcesUntilStoppedGuardCustody(t *testing.T) {
	for _, kind := range []string{"print-failure", "stop-failure", "held-state", "held-lifecycle", "foreign-launch", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			f := newDarwinPurgeFixture(t)
			if err := os.MkdirAll(f.paths.StateDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(f.paths.LaunchPath), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.paths.LaunchPath, []byte(darwinLaunchMarker), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(f.paths.HelperPath), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.paths.HelperPath, []byte("helper"), 0755); err != nil {
				t.Fatal(err)
			}
			if kind == "foreign-launch" {
				if err := os.WriteFile(f.paths.LaunchPath, []byte("foreign"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "held-state" || kind == "held-lifecycle" {
				name := "lock"
				if kind == "held-lifecycle" {
					name = "lifecycle.lock"
				}
				lock, err := lockState(filepath.Join(f.paths.StateDir, name))
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			}
			run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
				if name == "/bin/launchctl" && (kind == "print-failure" && args[0] == "print" || kind == "stop-failure" && args[0] == "bootout") {
					return nil, syscall.EIO
				}
				return f.run(ctx, name, args...)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if kind == "canceled" {
				cancel()
			}
			if err := purgeDarwin(ctx, darwinPurge{Paths: f.paths, Run: run, LocalNames: func(context.Context) error { return nil }}); err == nil {
				t.Fatal("unsafe or active guard accepted")
			}
			for _, path := range []string{f.paths.StateDir, f.paths.HelperPath, f.paths.LaunchPath} {
				if _, err := os.Lstat(path); err != nil {
					t.Fatal("guard resource removed before custody proof", err)
				}
			}
			if f.called("/sbin/pfctl -a " + f.paths.Anchor + " -F all") {
				t.Fatal("PF protection removed before stopped custody")
			}
		})
	}
}

func TestPurgeDarwinRetiresOwnedLegacyStateWithoutRemovingForeignState(t *testing.T) {
	for _, kind := range []string{"owned", "empty", "foreign-state", "bad-ranges", "state-symlink", "held-state", "foreign-control", "bad-certificate"} {
		t.Run(kind, func(t *testing.T) {
			f := newDarwinPurgeFixture(t)
			f.paths.LegacyStateDir = filepath.Join(filepath.Dir(f.paths.StateDir), "deviceguard")
			f.paths.LegacySocketDir = filepath.Join(filepath.Dir(f.paths.StateDir), "deviceguard-control")
			if err := os.MkdirAll(f.paths.LegacyStateDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(f.paths.LegacySocketDir, 0700); err != nil {
				t.Fatal(err)
			}
			if kind != "empty" {
				if err := writeLoopbackState(f.paths.LegacyStateDir, loopbackState{Active: "127.100.0.0/16", Protected: []string{"127.100.0.0/16"}}); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.paths.LegacyStateDir, "aliases.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"certificates", "trusted-ca"} {
					if err := os.Mkdir(filepath.Join(f.paths.LegacyStateDir, name), 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			switch kind {
			case "foreign-state":
				if err := os.WriteFile(filepath.Join(f.paths.LegacyStateDir, "foreign"), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			case "bad-ranges":
				if err := os.WriteFile(filepath.Join(f.paths.LegacyStateDir, "loopback-ranges.json"), []byte("invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			case "state-symlink":
				if err := os.Rename(f.paths.LegacyStateDir, f.paths.LegacyStateDir+".foreign"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.paths.LegacyStateDir+".foreign", f.paths.LegacyStateDir); err != nil {
					t.Fatal(err)
				}
			case "held-state":
				lock, err := lockState(filepath.Join(f.paths.LegacyStateDir, "lock"))
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			case "foreign-control":
				if err := os.WriteFile(filepath.Join(f.paths.LegacySocketDir, "foreign"), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			case "bad-certificate":
				dir := filepath.Join(f.paths.LegacyStateDir, "certificates", "local.example.test")
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "rootCA.pem"), []byte("foreign"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err := f.purge(ctx)
			if kind == "owned" || kind == "empty" {
				if err != nil {
					t.Fatal(err)
				}
				for _, path := range []string{f.paths.LegacyStateDir, f.paths.LegacySocketDir} {
					if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("legacy namespace remains", err)
					}
				}
				return
			}
			if err == nil {
				t.Fatal("foreign or unsafe legacy state accepted")
			}
			path := f.paths.LegacyStateDir
			if kind == "foreign-control" {
				path = filepath.Join(f.paths.LegacySocketDir, "foreign")
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("foreign or unsafe state removed", err)
			}
		})
	}
}

func (f *darwinPurgeFixture) called(command string) bool {
	for _, call := range f.calls {
		if call == command {
			return true
		}
	}
	return false
}

func TestPurgeDarwinRemovesStateSocketAndOwnedAccount(t *testing.T) {
	f := newDarwinPurgeFixture(t)
	f.paths.LegacyLaunchPath = filepath.Join(filepath.Dir(f.paths.LaunchPath), "io.paperboat.deviceguard.plist")
	f.paths.LegacyHelperPath = filepath.Join(filepath.Dir(f.paths.HelperPath), "io.paperboat.deviceguard")
	f.paths.LegacyAnchor = "com.apple/000.paperboat-deviceguard"
	for _, path := range []string{f.paths.LegacyLaunchPath, f.paths.LegacyHelperPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		body := []byte("helper")
		if path == f.paths.LegacyLaunchPath {
			body = []byte("<!-- Managed by Paperboat device guard -->")
		}
		if err := os.WriteFile(path, body, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(f.paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.paths.SocketDir, "control.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.dsclRecord[darwinBrokerAccount] = darwinAccountMarker

	if err := f.purge(context.Background()); err != nil {
		t.Fatalf("purge: %v", err)
	}
	for _, path := range []string{f.paths.StateDir, f.paths.SocketDir, f.paths.LegacyLaunchPath, f.paths.LegacyHelperPath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("%s still exists (err=%v)", path, err)
		}
	}
	if !f.called("/usr/bin/dscl . -delete /Users/" + darwinBrokerAccount) {
		t.Fatalf("owned account was not deleted; calls=%v", f.calls)
	}
	if !f.called("/sbin/pfctl -a com.apple/000.paperboat-machineguard -F all") {
		t.Fatalf("PF anchor was not flushed; calls=%v", f.calls)
	}
	if !f.called("/sbin/pfctl -a com.apple/000.paperboat-deviceguard -F all") || !f.called("/bin/launchctl bootout system/io.paperboat.deviceguard") {
		t.Fatalf("legacy device guard was not retired; calls=%v", f.calls)
	}
}

func TestPurgeDarwinPreservesForeignAccountAndRetainsState(t *testing.T) {
	f := newDarwinPurgeFixture(t)
	if err := os.MkdirAll(f.paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	f.dsclRecord[darwinBrokerAccount] = "Someone else"

	err := f.purge(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not owned by Paperboat") {
		t.Fatalf("purge error = %v, want a foreign account report", err)
	}
	if f.called("/usr/bin/dscl . -delete /Users/" + darwinBrokerAccount) {
		t.Fatalf("foreign account was deleted")
	}
	if _, statErr := os.Stat(f.paths.StateDir); statErr != nil {
		t.Fatalf("state was removed after a failure: %v", statErr)
	}
}

func TestDarwinRetiredPlatformStateReportsLeftovers(t *testing.T) {
	state := t.TempDir()
	if err := darwinRetiredPlatformState(state); err != nil {
		t.Fatalf("empty state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(state, "aliases.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := darwinRetiredPlatformState(state); err != nil {
		t.Fatalf("empty alias journal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(state, "aliases.json"), []byte(`{"127.100.0.2":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := darwinRetiredPlatformState(state); err == nil {
		t.Fatal("remaining loopback alias was not reported")
	}
	if err := os.WriteFile(filepath.Join(state, "pf-enable-token"), []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := darwinRetiredPlatformState(state); err == nil {
		t.Fatal("remaining PF enable reference was not reported")
	}
}

// Exercise real security enumeration/deletion in a task-owned keychain. Purge
// has no retained CA journal, exactly as after interrupted historical cleanup.
func TestPurgeDarwinRetiresOrphanCertificatesInIsolatedKeychain(t *testing.T) {
	root := t.TempDir()
	keychain := filepath.Join(root, "isolated.keychain-db")
	if err := exec.Command("/usr/bin/security", "create-keychain", "-p", "task-owned-fixture", keychain).Run(); err != nil {
		t.Fatal("create isolated keychain", err)
	}
	t.Cleanup(func() {
		if err := exec.Command("/usr/bin/security", "delete-keychain", keychain).Run(); err != nil {
			t.Error("delete isolated keychain", err)
		}
	})
	if err := exec.Command("/usr/bin/security", "unlock-keychain", "-p", "task-owned-fixture", keychain).Run(); err != nil {
		t.Fatal(err)
	}
	owned, err := splitdns.LoadOrCreateConstrainedCA(filepath.Join(root, "historical"), "local.example.test")
	if err != nil {
		t.Fatal(err)
	}
	second, err := splitdns.LoadOrCreateConstrainedCA(filepath.Join(root, "second"), "private.example")
	if err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "Paperboat Local Root CA", Organization: []string{"Foreign Desktop CA"}}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	foreign := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	for i, raw := range [][]byte{owned.CertPEM(), second.CertPEM(), foreign} {
		path := filepath.Join(root, fmt.Sprintf("cert-%d.pem", i))
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := exec.Command("/usr/bin/security", "import", path, "-k", keychain).Run(); err != nil {
			t.Fatal("import fixture certificate", err)
		}
	}
	f := newDarwinPurgeFixture(t)
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "/usr/bin/security" {
			args = append([]string(nil), args...)
			args[len(args)-1] = keychain
			return exec.CommandContext(ctx, name, args...).Output()
		}
		return f.run(ctx, name, args...)
	}
	if err := purgeDarwin(context.Background(), darwinPurge{Paths: f.paths, Run: run, LocalNames: func(context.Context) error { return nil }}); err != nil {
		t.Fatal("purge orphan trust", err)
	}
	if _, err := os.Lstat(f.paths.StateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("orphan purge retained newly provisioned lock namespace", err)
	}
	output, err := exec.Command("/usr/bin/security", "find-certificate", "-a", "-p", keychain).Output()
	if err != nil {
		t.Fatal(err)
	}
	var certificates [][]byte
	for len(bytes.TrimSpace(output)) > 0 {
		block, rest := pem.Decode(output)
		if block == nil {
			t.Fatal("invalid keychain certificate output")
		}
		certificates = append(certificates, block.Bytes)
		output = rest
	}
	if len(certificates) != 1 || !bytes.Equal(certificates[0], der) {
		t.Fatalf("purge retained owned orphan or removed foreign certificate: certificates=%d", len(certificates))
	}
}
