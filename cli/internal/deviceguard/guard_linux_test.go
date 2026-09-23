//go:build linux

package deviceguard

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/miekg/dns"
	"golang.org/x/sys/unix"
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
)

func TestDeviceGuardValidation(t *testing.T) {
	for _, name := range []string{"office.pprbt", "studio-1.mydev"} {
		if !validName(name) {
			t.Fatalf("valid name rejected: %s", name)
		}
	}
	for _, name := range []string{"office.com", "Office.pprbt", "a.b.pprbt", "office.local", "-bad.pprbt"} {
		if validName(name) {
			t.Fatalf("unsafe name accepted: %s", name)
		}
	}
	for _, address := range []string{"127.100.0.1", "127.0.0.1", "127.100.0.0", "127.100.2.255", "::1", "192.0.2.1"} {
		if validAddress(address) {
			t.Fatalf("unsafe address accepted: %s", address)
		}
	}
}

func TestDeviceGuardValidationUsesConfiguredCIDR(t *testing.T) {
	if !validAddressInCIDR("127.212.23.45", "127.212.0.0/16") {
		t.Fatal("configured device address rejected")
	}
	for _, address := range []string{"127.100.23.45", "127.212.0.1", "127.212.23.0", "127.212.23.255"} {
		if validAddressInCIDR(address, "127.212.0.0/16") {
			t.Errorf("reserved or foreign address %s accepted", address)
		}
	}
}

func TestRuntimeStatusReportsActiveOwnersForSafeRangeChange(t *testing.T) {
	g := &guardServer{cfg: Config{LoopbackCIDR: "127.212.0.0/16", DNSAddress: "127.212.0.1:53535"}, leases: map[string]*guardedLease{
		"first":  {uid: "1000"},
		"second": {uid: "1001"},
		"third":  {uid: "1001"},
	}}
	status := g.runtimeStatus()
	if !status.Ready || status.ActiveLeases != 3 || status.ActiveOwners != 2 || status.LoopbackCIDR != "127.212.0.0/16" {
		t.Fatalf("status=%+v", status)
	}
}

func TestLinuxProtectionRetainsPriorLoopbackRanges(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "rules")
	nft := filepath.Join(dir, "nft")
	if err := os.WriteFile(nft, []byte("#!/bin/sh\nif [ \"$1\" = \"-f\" ]; then cat > \"$CAPTURE\"; fi\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CAPTURE", capture)
	cfg := Config{LoopbackCIDR: "127.212.0.0/16", ProtectedLoopbackCIDRs: []string{"127.212.0.0/16", "127.100.0.0/16"}, DNSAddress: "127.212.0.1:53535"}
	if err := applyProtection(context.Background(), cfg, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	for _, cidr := range cfg.ProtectedLoopbackCIDRs {
		if !strings.Contains(string(data), cidr) {
			t.Errorf("nft rules omit protected range %s", cidr)
		}
	}
}
func TestLinuxDeviceGuardNamespace(t *testing.T) {
	if os.Getenv("PAPERBOAT_GUARD_PRIVILEGED_TEST") != "1" {
		t.Skip("requires approved Linux target and isolated network namespace")
	}
	if os.Geteuid() != 0 {
		t.Fatal("requires root to create an isolated network namespace")
	}
	namespace, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("PAPERBOAT_GUARD_PARENT_NETNS") == "" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(executable, "-test.run=^TestLinuxDeviceGuardNamespace$", "-test.v", "-test.timeout=30s")
		cmd.Env = append(os.Environ(), "PAPERBOAT_GUARD_PARENT_NETNS="+namespace)
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWNET}
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated namespace: %v\n%s", err, output)
		}
		t.Log(strings.TrimSpace(string(output)))
		return
	}
	if namespace == os.Getenv("PAPERBOAT_GUARD_PARENT_NETNS") {
		t.Fatal("refusing firewall test outside a separate network namespace")
	}
	if output, err := exec.Command("ip", "link", "set", "lo", "up").CombinedOutput(); err != nil {
		t.Fatalf("loopback: %v %s", err, output)
	}
	for _, tool := range []string{"nft", "ip"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	os.Chmod(filepath.Dir(root), 0755)
	os.Chmod(root, 0755)
	cfg := Config{StateDir: filepath.Join(root, "state"), Socket: filepath.Join(root, "run", "control.sock"), DNSAddress: "127.100.0.1:53535"}
	start := func() (*Client, context.CancelFunc, <-chan error) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- Serve(ctx, cfg) }()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			client, err := Connect(context.Background(), cfg.Socket)
			if err == nil {
				return client, cancel, done
			}
			select {
			case err := <-done:
				cancel()
				t.Fatalf("guard startup: %v", err)
			default:
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		t.Fatal("guard startup timed out")
		return nil, nil, nil
	}
	client, stop, done := start()
	defer func() {
		client.Close()
		stop()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("guard cleanup timed out")
		}
	}()
	address := netip.MustParseAddr("127.100.23.45")
	const port = 48086
	destination := net.JoinHostPort(address.String(), fmt.Sprint(port))
	listener, err := client.Acquire(t.Context(), "office.pprbt", address, port)
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	if err = client.ReplaceNames(t.Context(), map[string]netip.Addr{"office.pprbt": address}); err != nil {
		t.Fatal(err)
	}

	httpsLease, err := client.Acquire(t.Context(), "office.pprbt", address, 443)
	if err != nil {
		t.Fatal(err)
	}
	defer httpsLease.Close()
	if err := client.ReplaceAliases(t.Context(), map[string]string{"browser.pprbt": "office.pprbt"}); err != nil {
		t.Fatal(err)
	}
	if err := client.ReplaceNames(t.Context(), map[string]netip.Addr{"office.pprbt": address, "browser.pprbt": address}); err != nil {
		t.Fatal(err)
	}
	aliasQuestion := new(dns.Msg)
	aliasQuestion.SetQuestion("browser.pprbt.", dns.TypeA)
	aliasReply, _, aliasErr := (&dns.Client{Timeout: time.Second}).Exchange(aliasQuestion, cfg.DNSAddress)
	if aliasErr != nil || len(aliasReply.Answer) != 1 {
		t.Fatalf("flat alias DNS: %v %v", aliasReply, aliasErr)
	}
	aliasQuestion.SetQuestion("nested.office.pprbt.", dns.TypeA)
	aliasReply, _, aliasErr = (&dns.Client{Timeout: time.Second}).Exchange(aliasQuestion, cfg.DNSAddress)
	if aliasErr != nil || len(aliasReply.Answer) != 0 {
		t.Fatalf("nested DNS unexpectedly served: %v %v", aliasReply, aliasErr)
	}
	if err := client.ReplaceAliases(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if err := httpsLease.Close(); err != nil {
		t.Fatal(err)
	}
	question := new(dns.Msg)
	question.SetQuestion("office.pprbt.", dns.TypeA)
	reply, _, err := (&dns.Client{Timeout: time.Second}).Exchange(question, cfg.DNSAddress)
	if err != nil || len(reply.Answer) != 1 || !strings.Contains(reply.Answer[0].String(), address.String()) {
		t.Fatalf("published DNS: %v %v", reply, err)
	}
	echo := func() {
		t.Helper()
		conn, err := net.DialTimeout("tcp4", destination, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(time.Second))
		if _, err = conn.Write([]byte("private guard bytes")); err != nil {
			t.Fatal(err)
		}
		result := make([]byte, 19)
		if _, err = io.ReadFull(conn, result); err != nil || string(result) != "private guard bytes" {
			t.Fatalf("echo %q %v", result, err)
		}
	}
	echo()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	probeRoot := t.TempDir()
	os.Chmod(filepath.Dir(probeRoot), 0755)
	os.Chmod(probeRoot, 0755)
	probe := filepath.Join(probeRoot, "probe")
	source, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	target, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0755)
	if err != nil {
		source.Close()
		t.Fatal(err)
	}
	_, err = io.Copy(target, source)
	source.Close()
	target.Close()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(probe, "-test.run=^TestDeviceGuardOtherUserProbe$")
	cmd.Env = append(os.Environ(), "PAPERBOAT_GUARD_PROBE="+destination, "PAPERBOAT_GUARD_SOCKET="+cfg.Socket)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("foreign UID denial: %v %s", err, output)
	}
	if accepted.Load() != 1 {
		t.Fatal("unauthorized traffic reached accept")
	}
	denied := func() {
		t.Helper()
		conn, err := net.DialTimeout("tcp4", destination, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			t.Fatal("unprotected replacement reachable")
		}
	}
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	wildcard, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	denied()
	wildcard.Close()
	question.SetQuestion("office.pprbt.", dns.TypeA)
	reply, _, err = (&dns.Client{Timeout: time.Second}).Exchange(question, cfg.DNSAddress)
	if err != nil || reply.Rcode != dns.RcodeNameError {
		t.Fatalf("withdrawn DNS survived: %v %v", reply, err)
	}
	// A distinct controller cannot steal an active socket; old connection closure
	// never releases a replacement's listener. Recovery retains durable reservations.
	listener, err = client.Acquire(t.Context(), "office.pprbt", address, port)
	if err != nil {
		t.Fatal(err)
	}
	other, err := Connect(t.Context(), cfg.Socket)
	if err != nil {
		t.Fatal(err)
	}
	if extra, err := other.Acquire(t.Context(), "office.pprbt", address, port); err == nil {
		extra.Close()
		t.Fatal("second controller stole listener")
	}
	other.Close()
	client.Close()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp4", destination, 50*time.Millisecond)
		if err != nil {
			break
		}
		conn.Close()
		time.Sleep(10 * time.Millisecond)
	}
	denied()
	listener.Close()
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("guard stop hung")
	}
	wildcard, err = net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	denied()
	wildcard.Close()
	// Full permanent reservations must still permit their existing owner to reconnect.
	journalPath := filepath.Join(cfg.StateDir, "reservations.json")
	data, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	var journal reservations
	if err = json.Unmarshal(data, &journal); err != nil {
		t.Fatal(err)
	}
	for len(journal.Names) < 4096 {
		journal.Names[fmt.Sprintf("reserved%d.pprbt", len(journal.Names))] = "0"
	}
	data, err = json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(journalPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	// Restore default deny and root-owned journal after restart.
	client, stop, done = start()
	listener, err = client.Acquire(t.Context(), "office.pprbt", address, port)
	if err != nil {
		t.Fatal(err)
	}
	if extra, err := client.Acquire(t.Context(), "new.pprbt", netip.MustParseAddr("127.100.23.47"), port); err == nil {
		extra.Close()
		t.Fatal("per-user permanent quota bypassed")
	}
	// Keep the duplicate descriptor open while simulating the rule tool becoming
	// unavailable. Shared-socket shutdown must revoke admission even then.
	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", t.TempDir())
	defer os.Setenv("PATH", oldPath)
	if _, file, err := client.exchange(t.Context(), request{Operation: "release", IP: address.String(), Port: port}); err == nil {
		if file != nil {
			file.Close()
		}
		t.Fatal("rule failure was hidden")
	}
	denied()
	tcp := listener.(*ownedListener).Listener.(*net.TCPListener)
	tcp.SetDeadline(time.Now().Add(100 * time.Millisecond))
	if accepted, err := tcp.Accept(); err == nil {
		accepted.Close()
		t.Fatal("retained descriptor accepted after failed withdrawal")
	}
	listener.Close()
}
func TestDeviceGuardOtherUserProbe(t *testing.T) {
	destination := os.Getenv("PAPERBOAT_GUARD_PROBE")
	if destination == "" {
		t.Skip("subprocess fixture")
	}
	if os.Geteuid() != 65534 {
		t.Fatal("probe did not drop UID")
	}
	conn, err := net.DialTimeout("tcp4", destination, 200*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatal("foreign UID reached private listener")
	}
	client, err := Connect(t.Context(), os.Getenv("PAPERBOAT_GUARD_SOCKET"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if stolen, err := client.Acquire(t.Context(), "office.pprbt", netip.MustParseAddr("127.100.23.45"), 48086); err == nil {
		stolen.Close()
		t.Fatal("foreign owner stole reserved name/address")
	}
	otherIP := netip.MustParseAddr("127.100.23.46")
	listener, err := client.Acquire(t.Context(), "studio.mydev", otherIP, 48086)
	if err != nil {
		t.Fatalf("disjoint OS-user listener: %v", err)
	}
	defer listener.Close()
	if err = client.ReplaceNames(t.Context(), map[string]netip.Addr{"studio.mydev": otherIP}); err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() {
		accepted, err := listener.Accept()
		if err != nil {
			ready <- err
			return
		}
		defer accepted.Close()
		_, err = accepted.Write([]byte("owned"))
		ready <- err
	}()
	own, err := net.DialTimeout("tcp4", "127.100.23.46:48086", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	own.SetDeadline(time.Now().Add(time.Second))
	data := make([]byte, 5)
	if _, err = io.ReadFull(own, data); err != nil || string(data) != "owned" {
		t.Fatalf("own listener flow: %q %v", data, err)
	}
	if err = <-ready; err != nil {
		t.Fatal(err)
	}

}
