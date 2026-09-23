//go:build darwin

package deviceguard

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDarwinUninstallDenyChild(t *testing.T) {
	if len(os.Args) < 4 || os.Args[len(os.Args)-3] != "uninstall-deny-child" {
		return
	}
	state, anchor := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	darwinAnchor = anchor
	darwinPrefix = "127.100.253.71"
	darwinBrokerIdentity = func(context.Context) (uint32, uint32, error) { return 70, 70, nil }
	if err := serveDenyOnly(context.Background(), Config{StateDir: state, DNSAddress: "127.0.0.1:53"}); err != nil {
		t.Fatal(err)
	}
}
func TestDarwinUninstallOwnedLifecycle(t *testing.T) {
	if os.Getenv("PAPERBOAT_DARWIN_UNINSTALL_E2E") != "1" {
		t.Skip("explicit scoped macOS uninstall acceptance")
	}
	if os.Geteuid() != 0 {
		t.Fatal("requires root")
	}
	id := strconv.Itoa(os.Getpid())
	label := "io.paperboat.deviceguard.uninstall-review." + id
	anchor := "com.apple/000.000-paperboat-uninstall-review-" + id
	priorAnchor, priorPrefix, priorIdentity, priorResolver := darwinAnchor, darwinPrefix, darwinBrokerIdentity, darwinResolverDirectory
	darwinAnchor = anchor
	darwinPrefix = "127.100.253.71"
	darwinBrokerIdentity = func(context.Context) (uint32, uint32, error) { return 70, 70, nil }
	root := t.TempDir()
	state := filepath.Join(root, "state")
	darwinResolverDirectory = filepath.Join(root, "resolver")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(darwinResolverDirectory, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := darwinInstallConfig{HelperPath: filepath.Join("/Library/PrivilegedHelperTools", label), LaunchPath: filepath.Join("/Library/LaunchDaemons", label+".plist"), Label: label, Socket: filepath.Join("/var/run", label+".sock"), Account: "_unused_fixture", LaunchArguments: []string{"-test.run=TestDarwinInstallerReadinessChild", "--", "readiness-child", filepath.Join("/var/run", label+".sock")}}
	denyArgs := []string{"-test.run=TestDarwinUninstallDenyChild", "--", "uninstall-deny-child", state, anchor}
	guardCfg := Config{StateDir: state, DNSAddress: "127.0.0.1:53", ConfigureResolver: true}
	before, err := exec.Command("/sbin/pfctl", "-sr").Output()
	if err != nil {
		t.Fatal(err)
	}
	if existing, e := exec.Command("/sbin/pfctl", "-a", anchor, "-sr").Output(); e != nil || strings.TrimSpace(string(existing)) != "" {
		t.Fatal("fixture anchor not empty", e)
	}
	aliasExists, err := darwinAliasExists(t.Context(), "127.100.253.71")
	if err != nil || aliasExists {
		t.Fatal("fixture alias unavailable", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("/bin/launchctl", "bootout", "system/"+label).Run()
		_ = os.Remove(cfg.LaunchPath)
		_ = os.Remove(cfg.HelperPath)
		_ = os.Remove(cfg.Socket)
		_ = exec.Command("/sbin/ifconfig", "lo0", "-alias", "127.100.253.71").Run()
		retirePlatform(guardCfg)
		_ = exec.Command("/sbin/pfctl", "-a", anchor, "-F", "all").Run()
		after, e := exec.Command("/sbin/pfctl", "-sr").Output()
		if e != nil || string(before) != string(after) {
			t.Error("root PF policy changed", e)
		}
		darwinAnchor, darwinPrefix, darwinBrokerIdentity, darwinResolverDirectory = priorAnchor, priorPrefix, priorIdentity, priorResolver
	})
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err = installDarwin(ctx, executable, cfg); err != nil {
		t.Fatal(err)
	}
	// A foreign job must be rejected before stop or policy changes.
	original, err := os.ReadFile(cfg.LaunchPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cfg.LaunchPath, []byte("foreign"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = uninstallDarwin(ctx, cfg, guardCfg, denyArgs); err == nil {
		t.Fatal("foreign job removed")
	}
	if err = os.WriteFile(cfg.LaunchPath, original, 0644); err != nil {
		t.Fatal(err)
	}
	// Corrupt task-owned CA metadata causes a recoverable failure after deny.
	invalid := filepath.Join(state, "certificates", "invalid.public")
	if err = os.MkdirAll(invalid, 0700); err != nil {
		t.Fatal(err)
	}
	result, err := uninstallDarwin(ctx, cfg, guardCfg, denyArgs)
	if err == nil || len(result.Retained) == 0 {
		t.Fatalf("partial=%+v err=%v", result, err)
	}
	rules, e := exec.Command("/sbin/pfctl", "-a", anchor, "-sr").Output()
	if e != nil || !strings.Contains(string(rules), "block") {
		t.Fatal("failure lost deny", e)
	}
	if err = os.Remove(invalid); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		result, err = uninstallDarwin(ctx, cfg, guardCfg, denyArgs)
		if err != nil {
			t.Fatalf("uninstall %d: %v", i, err)
		}
		if len(result.Retained) != 3 {
			t.Fatalf("retained=%+v", result)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			lock, e := lockState(filepath.Join(state, "lock"))
			if e != nil {
				break
			}
			lock.Close()
			if time.Now().After(deadline) {
				t.Fatal("deny job did not own runtime lock")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	// Default denial still blocks a wildcard receiver at the cached address.
	if out, e := exec.Command("/sbin/ifconfig", "lo0", "alias", "127.100.253.71", "255.255.255.255").CombinedOutput(); e != nil {
		t.Fatalf("fixture alias: %v %s", e, out)
	}
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	address := fmt.Sprintf("127.100.253.71:%d", listener.Addr().(*net.TCPAddr).Port)
	if c, e := net.DialTimeout("tcp4", address, 200*time.Millisecond); e == nil {
		c.Close()
		t.Fatal("uninstall exposed cached address")
	}
	if err = installDarwin(ctx, executable, cfg); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	assertDarwinInstallerReady(t, cfg.Socket)
}
