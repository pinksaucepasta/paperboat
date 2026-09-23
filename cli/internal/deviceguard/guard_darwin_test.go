//go:build darwin

package deviceguard

import (
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

	"github.com/miekg/dns"
)

func TestDarwinGuardSocketChild(t *testing.T) {
	if os.Geteuid() == 0 || len(os.Args) < 2 {
		t.Skip("dedicated socket child only")
	}
	address := os.Args[len(os.Args)-1]
	loopbackCIDR := os.Args[len(os.Args)-2]
	if !strings.HasPrefix(address, "127.123.") {
		t.Skip("dedicated socket child only")
	}
	if err := ServeSocketChild(context.Background(), address, loopbackCIDR); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinGuardOtherUser(t *testing.T) {
	socket := os.Getenv("PAPERBOAT_GUARD_SOCKET")
	if socket == "" {
		t.Skip("isolated child only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if connection, err := net.DialTimeout("tcp4", "127.123.253.68:54369", 250*time.Millisecond); err == nil {
		connection.Close()
		t.Fatal("other owner reached guarded listener")
	}
	client, err := Connect(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if listener, err := client.Acquire(ctx, "office.pprbt", netip.MustParseAddr("127.123.253.68"), 54370); err == nil {
		listener.Close()
		t.Fatal("foreign name/address reservation stolen")
	}
	listener, err := client.Acquire(ctx, "other.pprbt", netip.MustParseAddr("127.123.253.69"), 54370)
	if err != nil {
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
	connection, err := net.DialTimeout("tcp4", "127.123.253.69:54370", time.Second)
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
	darwinPrefix = "{ 127.123.253.68 127.123.253.69 }"
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
	cfg := Config{StateDir: filepath.Join(root, "state"), Socket: filepath.Join(root, "control.sock"), DNSAddress: "127.0.0.1:53537", LoopbackCIDR: "127.123.0.0/16", ProtectedLoopbackCIDRs: []string{"127.100.0.0/16"}}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, address := range []string{"127.123.253.68", "127.123.253.69"} {
			if exists, _ := darwinAliasExists(ctx, address); exists {
				_, _ = darwinRun(ctx, "", "/sbin/ifconfig", "lo0", "-alias", address)
			}
		}
		if _, err := darwinRun(ctx, "", "/sbin/pfctl", "-a", darwinAnchor, "-F", "rules"); err != nil {
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
	address := netip.MustParseAddr("127.123.253.68")
	listener, err := client.Acquire(ctx, "office.pprbt", address, 54369)
	if err != nil {
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
	if err = client.ReplaceNames(ctx, map[string]netip.Addr{"office.pprbt": address}); err != nil {
		t.Fatal(err)
	}
	question := new(dns.Msg)
	question.SetQuestion("office.pprbt.", dns.TypeA)
	reply, _, err := (&dns.Client{Timeout: time.Second}).Exchange(question, cfg.DNSAddress)
	if err != nil || len(reply.Answer) != 1 {
		t.Fatal("guard DNS", reply, err)
	}
	echo := func() {
		t.Helper()
		connection, err := net.DialTimeout("tcp4", "127.123.253.68:54369", time.Second)
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
	child := exec.Command(os.Args[0], "-test.run=^TestDarwinGuardOtherUser$", "-test.v")
	child.Env = append(os.Environ(), "PAPERBOAT_GUARD_SOCKET="+cfg.Socket)
	child.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 501, Gid: 20}}
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("other OS user: %v %s", err, output)
	}
	if accepted.Load() != 1 {
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
	if connection, err := net.DialTimeout("tcp4", "127.123.253.68:54369", 250*time.Millisecond); err == nil {
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
		t.Fatal("retained previous range reached wildcard listener")
	}
	if err = client.ReplaceNames(ctx, nil); err != nil {
		t.Fatal(err)
	}
	reply, _, err = (&dns.Client{Timeout: time.Second}).Exchange(question, cfg.DNSAddress)
	if err != nil || len(reply.Answer) != 0 {
		t.Fatal("DNS withdrawal", reply, err)
	}
	t.Log(fmt.Sprintf("alternate active range passed; retained previous range denied; root caller and broker UID70 bytes passed; UID501 denied and disjoint ownership passed; %d authorized accepts", accepted.Load()))
}
