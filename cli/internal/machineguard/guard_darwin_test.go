//go:build darwin

package machineguard

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/splitdns"
)

func TestDarwinGuardSocketChild(t *testing.T) {
	if os.Geteuid() == 0 || len(os.Args) < 2 {
		t.Skip("dedicated socket child only")
	}
	address := os.Args[len(os.Args)-1]
	loopbackCIDR := os.Args[len(os.Args)-2]
	if !strings.HasPrefix(address, "127.100.") {
		t.Skip("dedicated socket child only")
	}
	if err := ServeSocketChild(context.Background(), address, loopbackCIDR); err != nil {
		t.Fatal(err)
	}
}

// TestDarwinGuardProgramBind is the other user's program: it binds 0.0.0.0 on the port, says "bound" and holds the
// socket until its stdin closes.
func TestDarwinGuardProgramBind(t *testing.T) {
	port := os.Getenv("PAPERBOAT_GUARD_PROGRAM_PORT")
	if port == "" {
		t.Skip("isolated child only")
	}
	listener, err := net.Listen("tcp4", "0.0.0.0:"+port)
	if err != nil {
		fmt.Println("bind failed:", err)
		t.Fatal(err)
	}
	defer listener.Close()
	fmt.Println("bound")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestDarwinGuardOtherUser(t *testing.T) {
	socket := os.Getenv("PAPERBOAT_GUARD_SOCKET")
	if socket == "" {
		t.Skip("isolated child only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if connection, err := net.DialTimeout("tcp4", "127.100.253.68:54369", 250*time.Millisecond); err == nil {
		connection.Close()
		t.Fatal("other owner reached guarded listener")
	}
	client, err := Connect(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if listener, err := client.Acquire(ctx, "office.local.example.test", netip.MustParseAddr("127.100.253.68"), 54370); err == nil {
		listener.Close()
		t.Fatal("foreign name/address reservation stolen")
	}
	if _, err = client.Status(ctx); err != nil {
		t.Fatal(err)
	}
	listener, assigned, err := client.AcquireMachine(ctx, "account-2", "machine-2", "other.local.example.test", netip.MustParseAddr("127.100.253.69"), 54370)
	if err != nil {
		t.Fatal(err)
	}
	if assigned.String() != "127.100.253.69" {
		t.Fatal("guard assigned another address", assigned)
	}
	if err = client.ApplyPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			defer connection.Close()
			_, _ = io.Copy(connection, connection)
		}
	}()
	connection, err := net.DialTimeout("tcp4", "127.100.253.69:54370", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(time.Second))
	_, err = connection.Write([]byte("user"))
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 4)
	if _, err = io.ReadFull(connection, data); err != nil || string(data) != "user" {
		t.Fatal("coexisting owner echo", err)
	}
}

func TestDarwinGuardKernel(t *testing.T) {
	if os.Getenv("PAPERBOAT_GUARD_DARWIN_TEST") != "1" {
		t.Skip("requires explicitly authorized macOS PF target")
	}
	if os.Geteuid() != 0 {
		t.Fatal("root required")
	}
	darwinAnchor = "com.apple/000.paperboat-review-guard"
	darwinPrefix = "{ 127.100.253.68 127.100.253.69 }"
	darwinBrokerIdentity = func(context.Context) (uint32, uint32, error) { return 70, 70, nil }
	darwinSocketCommand = func(ctx context.Context, address, loopbackCIDR string) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDarwinGuardSocketChild$", "--", loopbackCIDR, address), nil
	}
	existing, checkErr := exec.Command("/sbin/pfctl", "-a", darwinAnchor, "-sr").Output()
	if checkErr != nil || strings.TrimSpace(string(existing)) != "" {
		t.Fatal("test anchor already exists or cannot be inspected", checkErr)
	}
	before, err := darwinRun(t.Context(), "", "/sbin/pfctl", "-sr")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "pb-guard-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if err = os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{StateDir: filepath.Join(root, "state"), Socket: filepath.Join(root, "control.sock"), LoopbackCIDR: "127.100.0.0/16"}
	recordTestDefaultDomain(t, cfg.StateDir)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, address := range []string{"127.100.253.68", "127.100.253.69"} {
			if exists, _ := darwinAliasExists(ctx, address); exists {
				_, _ = darwinRun(ctx, "", "/sbin/ifconfig", "lo0", "-alias", address)
			}
		}
		if _, err := darwinRun(ctx, "", "/sbin/pfctl", "-a", darwinAnchor, "-F", "all"); err != nil {
			t.Error(err)
		}
		after, err := darwinRun(ctx, "", "/sbin/pfctl", "-sr")
		if err != nil || string(after) != string(before) {
			t.Error("unrelated root PF rules changed", err)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, cfg) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			t.Error("guard failed to stop")
		}
	}()
	var client *Client
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		client, err = Connect(ctx, cfg.Socket)
		if err == nil {
			break
		}
		select {
		case err := <-done:
			done <- err
			t.Fatal("guard startup", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err = client.Status(ctx); err != nil { // learns the guard's claims feature
		t.Fatal(err)
	}
	listener, address, err := client.AcquireMachine(ctx, "account-1", "machine-1", "office.local.example.test", netip.MustParseAddr("127.100.253.68"), 54369)
	if err != nil {
		t.Fatal(err)
	}
	if address.String() != "127.100.253.68" {
		t.Fatal("guard assigned another address", address)
	}
	if err = client.ApplyPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() { defer connection.Close(); _, _ = io.Copy(connection, connection) }()
		}
	}()
	if err = client.ReplaceNames(ctx, map[string]netip.Addr{"office.local.example.test": address}); err != nil {
		t.Fatal(err)
	}
	echo := func() {
		t.Helper()
		connection, err := net.DialTimeout("tcp4", "127.100.253.68:54369", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		connection.SetDeadline(time.Now().Add(time.Second))
		_, err = connection.Write([]byte("guard"))
		if err != nil {
			t.Fatal(err)
		}
		data := make([]byte, 5)
		if _, err = io.ReadFull(connection, data); err != nil || string(data) != "guard" {
			t.Fatal("guard echo", err)
		}
	}
	echo()
	// A program of another OS user (Vite --host, docker -p) can still bind the machine port on every address, and
	// the protected name still reaches the guard, not the program.
	program := exec.Command(os.Args[0], "-test.run=^TestDarwinGuardProgramBind$", "-test.v")
	program.Env = append(os.Environ(), "PAPERBOAT_GUARD_PROGRAM_PORT=54369")
	program.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 501, Gid: 20}}
	hold, err := program.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	bound, err := program.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = program.Start(); err != nil {
		t.Fatal(err)
	}
	reader, line := bufio.NewReader(bound), ""
	for !strings.HasPrefix(line, "bound") && !strings.HasPrefix(line, "bind failed") { // after go test's own lines
		next, err := reader.ReadString('\n')
		if line = next; err != nil {
			break
		}
	}
	if !strings.HasPrefix(line, "bound") {
		hold.Close()
		program.Wait()
		t.Fatalf("a program cannot bind the machine port on every address: %s", line)
	}
	echo()
	hold.Close()
	if err = program.Wait(); err != nil {
		t.Fatal("program child", err)
	}
	const port = 54369
	// The same holds for a browser gateway port (https://<machine>.<domain>:<port>).
	gatewayPort := port + 1
	gateway, err := client.Acquire(ctx, splitdns.BrowserGatewayHostname, netip.MustParseAddr(splitdns.BrowserGatewayIP), gatewayPort)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.ApplyPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	gatewayProgram := exec.Command(os.Args[0], "-test.run=^TestDarwinGuardProgramBind$")
	gatewayProgram.Env = append(os.Environ(), fmt.Sprintf("PAPERBOAT_GUARD_PROGRAM_PORT=%d", gatewayPort))
	gatewayProgram.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 501, Gid: 20}}
	gatewayProgram.Stdin = strings.NewReader("") // binds, says bound, exits
	if output, err := gatewayProgram.CombinedOutput(); err != nil || !strings.Contains(string(output), "bound") {
		t.Fatalf("a program cannot bind a gateway port on every address: %v %s", err, output)
	}
	if err = gateway.Close(); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestDarwinGuardOtherUser$", "-test.v")
	child.Env = append(os.Environ(), "PAPERBOAT_GUARD_SOCKET="+cfg.Socket)
	child.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 501, Gid: 20}}
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("other OS user: %v %s", err, output)
	}
	if accepted.Load() != 2 { // the owner's two echoes
		t.Fatal("foreign traffic reached accept", accepted.Load())
	}
	echo()
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	// Closing the protected lease retires its alias; an unrelated wildcard must
	// not inherit cached traffic, even when the caller is the former owner.
	wildcard, err := net.Listen("tcp4", "0.0.0.0:54369")
	if err != nil {
		t.Fatal(err)
	}
	defer wildcard.Close()
	if connection, err := net.DialTimeout("tcp4", "127.100.253.68:54369", 250*time.Millisecond); err == nil {
		connection.Close()
		t.Fatal("cached address reached wildcard replacement")
	}
	retainedWildcard, err := net.Listen("tcp4", "0.0.0.0:54371")
	if err != nil {
		t.Fatal(err)
	}
	defer retainedWildcard.Close()
	if connection, err := net.DialTimeout("tcp4", "127.100.253.67:54371", 250*time.Millisecond); err == nil {
		connection.Close()
		t.Fatal("retired lease address reached wildcard listener")
	}
	if err = client.ReplaceNames(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("fixed 127.100/16 range passed; root caller and broker UID70 bytes passed; UID501 denied and disjoint ownership passed; %d authorized accepts", accepted.Load())
}
