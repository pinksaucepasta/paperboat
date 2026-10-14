//go:build darwin || linux

package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Exercise the production kernel peer check with a root listener, while the
// request/response fixture remains in the ordinary test process.
func updaterFixtureRootSocket(t *testing.T, target string) string {
	t.Helper()
	if os.Geteuid() == 0 {
		return target
	}
	path := filepath.Join(commandRuntimeTestRoot(t), "updater.sock")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	process := exec.Command("sudo", "-n", "env", "PB_TEST_UPDATER_PROXY="+path,
		"PB_TEST_UPDATER_TARGET="+target,
		"PB_TEST_UPDATER_UID="+strconv.Itoa(os.Geteuid()),
		"PB_TEST_UPDATER_GID="+strconv.Itoa(os.Getegid()), binary,
		"-test.run=^TestUpdaterRootFixtureProcess$", "-test.timeout=2m")
	input, err := process.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := process.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	process.Stderr = &stderr
	if err := process.Start(); err != nil {
		t.Fatalf("start root updater fixture: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	t.Cleanup(func() {
		_ = input.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("root updater fixture: %v (%s)", err, stderr.String())
			}
		case <-time.After(5 * time.Second):
			_ = exec.Command("sudo", "-n", "kill", "-TERM", strconv.Itoa(process.Process.Pid)).Run()
			t.Error("root updater fixture did not stop")
		}
	})
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		ready <- scanner.Scan() && scanner.Text() == "READY"
		_, _ = io.Copy(io.Discard, output)
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("root updater fixture did not become ready; passwordless sudo is required")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("root updater fixture startup timed out")
	}
	return path
}

func TestUpdaterRootFixtureProcess(t *testing.T) {
	path := os.Getenv("PB_TEST_UPDATER_PROXY")
	if path == "" {
		t.Skip("subprocess entry point for real root peer fixtures")
	}
	if os.Geteuid() != 0 {
		t.Fatal("updater fixture requires a root process")
	}
	uid, err := strconv.Atoi(os.Getenv("PB_TEST_UPDATER_UID"))
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(os.Getenv("PB_TEST_UPDATER_GID"))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chown(path, uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		_ = listener.Close()
	}()
	_, _ = io.WriteString(os.Stdout, "READY\n")
	for {
		connection, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		forwardUpdaterFixture(t, connection, os.Getenv("PB_TEST_UPDATER_TARGET"))
	}
}

func forwardUpdaterFixture(t *testing.T, connection *net.UnixConn, target string) {
	t.Helper()
	defer connection.Close()
	peer, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: target, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	deadline := time.Now().Add(5 * time.Second)
	_ = connection.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		_, _ = io.Copy(peer, connection)
		_ = peer.CloseWrite()
	}()
	_, _ = io.Copy(connection, peer)
	_ = connection.CloseWrite()
	workers.Wait()
}
