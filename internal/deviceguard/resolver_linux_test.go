//go:build linux

package deviceguard

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/sys/unix"
)

func TestLinuxGuardSystemResolver(t *testing.T) {
	if os.Getenv("PAPERBOAT_GUARD_PRIVILEGED_TEST") != "1" {
		t.Skip("requires approved isolated Linux target")
	}
	namespace, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("PAPERBOAT_RESOLVER_PARENT") == "" {
		executable, _ := os.Executable()
		cmd := exec.Command(executable, "-test.run=^TestLinuxGuardSystemResolver$", "-test.v", "-test.timeout=45s")
		cmd.Env = append(os.Environ(), "PAPERBOAT_RESOLVER_PARENT="+namespace)
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWNET | unix.CLONE_NEWNS}
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated resolver: %v\n%s", err, output)
		}
		t.Log(string(output))
		return
	}
	if namespace == os.Getenv("PAPERBOAT_RESOLVER_PARENT") {
		t.Fatal("refusing shared network namespace")
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount("tmpfs", "/run", "tmpfs", 0, "mode=0755"); err != nil {
		t.Fatal(err)
	}
	// sysfs was mounted by the parent network namespace. Remount it in this
	// private mount namespace so interface ownership metadata follows the
	// isolated network namespace as it does on a real host.
	if err := unix.Mount("sysfs", "/sys", "sysfs", 0, ""); err != nil {
		t.Fatal(err)
	}
	command := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", name, err, out)
		}
	}
	command("ip", "link", "set", "lo", "up")
	command("ip", "link", "add", "fixture-uplink", "type", "dummy")
	command("ip", "address", "add", "192.0.2.1/24", "dev", "fixture-uplink")
	command("ip", "link", "set", "fixture-uplink", "up")
	root := t.TempDir()
	config := filepath.Join(root, "resolved.conf")
	if err := os.WriteFile(config, []byte("[Resolve]\nDNS=127.0.0.60:5353\nFallbackDNS=\nDNSStubListener=yes\nLLMNR=no\nMulticastDNS=no\nDNSSEC=no\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount(config, "/etc/systemd/resolved.conf", "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll("/run/dbus", 0755)
	os.MkdirAll("/run/systemd", 0755)
	packet, err := net.ListenPacket("udp4", "127.0.0.60:5353")
	if err != nil {
		t.Fatal(err)
	}
	ordinary := &dns.Server{PacketConn: packet, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		r := new(dns.Msg)
		r.SetReply(q)
		for _, question := range q.Question {
			if question.Name == "ordinary.example." && question.Qtype == dns.TypeA {
				r.Answer = append(r.Answer, &dns.A{Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 1}, A: net.ParseIP("192.0.2.44")})
			}
		}
		_ = w.WriteMsg(r)
	})}
	go ordinary.ActivateAndServe()
	defer ordinary.Shutdown()
	startProcess := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		log, err := os.Create(filepath.Join(root, filepath.Base(name)+".log"))
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout = log
		cmd.Stderr = log
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			log.Close()
			if t.Failed() {
				data, _ := os.ReadFile(log.Name())
				t.Logf("%s: %s", name, data)
			}
		})
	}
	startProcess("dbus-daemon", "--system", "--nofork", "--nopidfile")
	for i := 0; i < 100; i++ {
		if _, err := os.Stat("/run/dbus/system_bus_socket"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	startProcess("/usr/lib/systemd/systemd-resolved")
	lookup := func(name, want string) error {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "getent", "ahostsv4", name).CombinedOutput()
		if err != nil || !strings.Contains(string(out), want) {
			return fmt.Errorf("lookup %s: %v %s", name, err, out)
		}
		return nil
	}
	for i := 0; i < 100; i++ {
		err = lookup("ordinary.example", "192.0.2.44")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		for _, args := range [][]string{{"resolvectl", "status"}, {"cat", "/etc/resolv.conf"}, {"resolvectl", "query", "ordinary.example"}} {
			out, e := exec.Command(args[0], args[1:]...).CombinedOutput()
			t.Logf("diagnostic %v: %v %s", args, e, out)
		}
		t.Fatal(err)
	}
	// Model the state left by a SIGKILL: the root-owned link and its resolved
	// routing domain survive, while the helper process and DNS listener do not.
	command("ip", "link", "add", "name", "paperboat-dns", "type", "dummy")
	command("ip", "link", "set", "dev", "paperboat-dns", "alias", "paperboat-deviceguard-v1")
	command("ip", "link", "set", "paperboat-dns", "up")
	command("ip", "address", "add", "127.100.0.1/32", "dev", "paperboat-dns", "scope", "global")
	command("resolvectl", "dns", "paperboat-dns", "127.100.0.1:53535")
	command("resolvectl", "default-route", "paperboat-dns", "no")
	command("resolvectl", "domain", "paperboat-dns", "~stale-before-restart")
	stale, err := exec.Command("resolvectl", "domain", "paperboat-dns").CombinedOutput()
	if err != nil || !strings.Contains(string(stale), "~stale-before-restart") {
		t.Fatalf("seed stale resolver projection: %v %s", err, stale)
	}
	cfg := Config{StateDir: filepath.Join(root, "state"), Socket: filepath.Join(root, "control", "guard.sock"), DNSAddress: "127.100.0.1:53535", ConfigureResolver: true}
	notifyPath := filepath.Join(root, "notify.sock")
	notify, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: notifyPath, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer notify.Close()
	t.Setenv("NOTIFY_SOCKET", notifyPath)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, cfg) }()
	defer func() {
		cancel()
		select {
		case result := <-done:
			if t.Failed() {
				t.Logf("guard shutdown: %v", result)
			}
		case <-time.After(5 * time.Second):
			t.Error("guard cleanup timeout")
		}
	}()
	if err = notify.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ready := make([]byte, 32)
	for {
		n, _, readyErr := notify.ReadFromUnix(ready)
		if readyErr != nil {
			t.Fatalf("guard readiness: %v", readyErr)
		}
		// resolvectl links libsystemd and may emit its own process status when
		// it inherits NOTIFY_SOCKET. Only READY=1 establishes helper readiness.
		if string(ready[:n]) == "READY=1" {
			break
		}
	}
	var client *Client
	for i := 0; i < 200; i++ {
		client, err = Connect(t.Context(), cfg.Socket)
		if err == nil {
			break
		}
		select {
		case e := <-done:
			t.Fatal(e)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	domains, err := exec.Command("resolvectl", "domain", "paperboat-dns").CombinedOutput()
	if err != nil {
		t.Fatalf("read restarted resolver projection: %v %s", err, domains)
	}
	if strings.Contains(string(domains), "~stale-before-restart") {
		t.Fatalf("stale resolver projection survived readiness: %s", domains)
	}
	defer client.Close()
	address := netip.MustParseAddr("127.100.23.45")
	listener, err := client.Acquire(t.Context(), "office.pprbt", address, 48087)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err = client.ReplaceNames(t.Context(), map[string]netip.Addr{"office.pprbt": address}); err != nil {
		t.Fatal(err)
	}
	if err = lookup("office.pprbt", address.String()); err != nil {
		t.Fatal(err)
	}
	if err = lookup("ordinary.example", "192.0.2.44"); err != nil {
		t.Fatal(err)
	}
	// Keep trust changes inside this mount namespace, including package hooks.
	for _, directory := range []string{"/etc/ssl/certs", "/usr/local/share/ca-certificates", "/etc/ca-certificates/update.d"} {
		if err := unix.Mount("tmpfs", directory, "tmpfs", 0, "mode=0755"); err != nil {
			t.Fatal(err)
		}
	}
	httpsListener, err := client.Acquire(t.Context(), "office.pprbt", address, 443)
	if err != nil {
		t.Fatal(err)
	}
	defer httpsListener.Close()
	bundle, err := client.Certificate(t.Context(), "office.pprbt")
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(bundle.CertificatePEM, bundle.PrivateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	httpsServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("trusted private response")) }), ReadHeaderTimeout: time.Second}
	defer httpsServer.Close()
	go httpsServer.Serve(tls.NewListener(httpsListener, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}))
	output, err := exec.Command("curl", "--noproxy", "*", "--fail", "--silent", "--show-error", "--max-time", "5", "https://office.pprbt/").CombinedOutput()
	if err != nil || string(output) != "trusted private response" {
		t.Fatalf("native system-trusted HTTPS: %v %s", err, output)
	}
	if err = client.ReplaceNames(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	command("resolvectl", "flush-caches")
	if err = lookup("office.pprbt", address.String()); err == nil {
		t.Fatal("withdrawn name still resolves")
	}
	if err = lookup("ordinary.example", "192.0.2.44"); err != nil {
		t.Fatal(err)
	}
}
