//go:build darwin

package deviceguard

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDarwinInstallerLaunchdLifecycle(t *testing.T) {
	if os.Getenv("PAPERBOAT_DARWIN_INSTALLER_E2E") != "1" {
		t.Skip("set PAPERBOAT_DARWIN_INSTALLER_E2E=1 and run as root on macOS")
	}
	if os.Geteuid() != 0 {
		t.Fatal("installer lifecycle test requires root")
	}
	id := strconv.Itoa(os.Getpid())
	label := "io.paperboat.deviceguard.review." + id
	socket := filepath.Join("/var/run", "paperboat-deviceguard-review-"+id+".sock")
	helper := filepath.Join("/Library/PrivilegedHelperTools", label)
	plist := filepath.Join("/Library/LaunchDaemons", label+".plist")
	account := "_paperboat_review_" + id
	cfg := darwinInstallConfig{
		HelperPath: helper, LaunchPath: plist, Label: label, Socket: socket, Account: account,
		LaunchArguments: []string{"-test.run=TestDarwinInstallerReadinessChild", "--", "readiness-child", socket},
		InstallAccount:  false,
	}
	cleanup := func() {
		_ = exec.Command("/bin/launchctl", "bootout", "system/"+label).Run()
		_ = os.Remove(plist)
		_ = os.Remove(helper)
		_ = os.Remove(socket)
		// Account creation is disabled for this fixture. Remove it defensively
		// only if the task-owned name somehow appeared.
		_ = exec.Command("/usr/bin/dscl", ".", "-delete", "/Users/"+account).Run()
	}
	cleanup()
	t.Cleanup(cleanup)

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := installDarwin(ctx, executable, cfg); err != nil {
		t.Fatalf("first install: %v", err)
	}
	assertDarwinInstallerReady(t, socket)
	if err := installDarwin(ctx, executable, cfg); err != nil {
		t.Fatalf("restart install: %v", err)
	}
	assertDarwinInstallerReady(t, socket)
	if output, err := exec.Command("/usr/bin/dscl", ".", "-list", "/Users").Output(); err != nil || strings.Contains("\n"+string(output)+"\n", "\n"+account+"\n") {
		t.Fatalf("fixture created an account despite InstallAccount=false: err=%v", err)
	}
}

func assertDarwinInstallerReady(t *testing.T, socket string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client, err := Connect(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.ReplaceNames(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinInstallerReadinessChild(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "readiness-child" {
		return
	}
	socket := os.Args[len(os.Args)-1]
	_ = os.Remove(socket)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go serveDarwinInstallerReadiness(conn)
	}
}

func serveDarwinInstallerReadiness(conn net.Conn) {
	defer conn.Close()
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return
	}
	for {
		data, err := readFrame(unixConn)
		if err != nil {
			return
		}
		var in request
		if json.Unmarshal(data, &in) != nil || in.Operation != "names" || len(in.Names) != 0 {
			return
		}
		if _, _, err := unixConn.WriteMsgUnix([]byte{0}, nil, nil); err != nil {
			return
		}
		out, _ := json.Marshal(response{})
		if writeFrame(unixConn, out) != nil {
			return
		}
	}
}
