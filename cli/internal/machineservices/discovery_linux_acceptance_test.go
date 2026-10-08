//go:build linux

package machineservices

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestSnapshotExcludesForeignUIDListener(t *testing.T) {
	if os.Getenv("PAPERBOAT_FOREIGN_UID_LISTENER") == "1" {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			os.Exit(2)
		}
		defer listener.Close()
		neighbor, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			os.Exit(2)
		}
		defer neighbor.Close()
		fmt.Println(listener.Addr().(*net.TCPAddr).Port, neighbor.Addr().(*net.TCPAddr).Port)
		_, _ = bufio.NewReader(os.Stdin).ReadByte()
		os.Exit(0)
	}
	if os.Geteuid() != 0 {
		t.Skip("requires root to create a listener under a different UID")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestSnapshotExcludesForeignUIDListener$")
	cmd.Env = append(os.Environ(), "PAPERBOAT_FOREIGN_UID_LISTENER=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(line)
	if len(fields) != 2 {
		t.Fatal("invalid foreign listener ports")
	}
	port, err := strconv.ParseUint(fields[0], 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	neighborPort, err := strconv.ParseUint(fields[1], 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	services, err := Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, service := range services {
		if service.Port == uint16(port) || service.Port == uint16(neighborPort) {
			t.Fatalf("foreign UID listener was advertised: %#v", service)
		}
	}
	explicit, err := ProbeExplicit(context.Background(), []Service{{Port: uint16(port), Loopback: "127.0.0.1"}})
	if err != nil || len(explicit) != 1 || explicit[0].Port != uint16(port) {
		t.Fatalf("explicit foreign listener denied: %#v %v", explicit, err)
	}
	// Default discovery remains unchanged after an explicit probe.
	services, err = Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, service := range services {
		if service.Port == uint16(port) || service.Port == uint16(neighborPort) {
			t.Fatal("explicit probe broadened automatic discovery")
		}
	}

}
