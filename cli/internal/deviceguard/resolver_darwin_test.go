//go:build darwin

package deviceguard

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestDarwinGuardSystemResolver(t *testing.T) {
	if os.Getenv("PAPERBOAT_GUARD_DARWIN_TEST") != "1" {
		t.Skip("requires authorized macOS resolver target")
	}
	if os.Geteuid() != 0 {
		t.Fatal("root required")
	}
	// Never let a test replace another running helper's resolver projections.
	entries, err := os.ReadDir(darwinResolverDirectory)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, _ := os.ReadFile(filepath.Join(darwinResolverDirectory, entry.Name()))
		if strings.HasPrefix(string(data), darwinResolverMarker) {
			t.Skip("active Paperboat resolver belongs to another task")
		}
	}
	path := filepath.Join(darwinResolverDirectory, "pbguardreview")
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("test resolver path occupied", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &dns.Server{PacketConn: conn, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		for _, q := range r.Question {
			if q.Qtype == dns.TypeA && q.Name == "office.pbguardreview." {
				m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 0}, A: net.ParseIP("127.100.253.68")})
			}
		}
		_ = w.WriteMsg(m)
	})}
	go server.ActivateAndServe()
	defer server.Shutdown()
	cfg := Config{DNSAddress: conn.LocalAddr().String(), ConfigureResolver: true}
	defer func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Error(err)
		}
	}()
	if err = setupResolver(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err = configureDomains(ctx, cfg, []string{"pbguardreview"}); err != nil {
		t.Fatal(err)
	}
	var output []byte
	for {
		output, err = exec.CommandContext(ctx, "/usr/bin/dscacheutil", "-q", "host", "-a", "name", "office.pbguardreview").CombinedOutput()
		if err == nil && strings.Contains(string(output), "127.100.253.68") {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("system resolver: %v %s", err, output)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err = configureDomains(ctx, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("resolver projection not retired", err)
	}
}
