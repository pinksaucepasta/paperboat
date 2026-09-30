//go:build linux || darwin || windows

package deviceguard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

func TestPublicBrowserAliasesRequireExactGatewayOwnership(t *testing.T) {
	owner := &certificateConnection{identity: "owner"}
	other := &certificateConnection{identity: "other"}
	g := &guardServer{cfg: Config{StateDir: t.TempDir(), LoopbackCIDR: "127.212.0.0/16"}, reserved: reservations{IPs: map[string]string{}, Names: map[string]string{}}, leases: map[string]*guardedLease{"https": {hostname: splitdns.BrowserGatewayHostname, ip: splitdns.BrowserGatewayIP, port: 443, owner: owner}}, names: map[controlConn]map[string]string{}, certificates: map[controlConn]map[string]cachedCertificate{}}
	name := "6767.hp." + splitdns.BrowserSuffix
	aliases := map[string]string{name: splitdns.BrowserGatewayHostname}
	if err := g.replaceAliases(t.Context(), other, "other", aliases); err == nil {
		t.Fatal("foreign connection borrowed gateway")
	}
	if err := g.replaceAliases(t.Context(), owner, "owner", aliases); err != nil {
		t.Fatal(err)
	}
	if g.certificateBase(owner, name) != splitdns.BrowserGatewayHostname || g.certificateBase(other, name) != "" || g.certificateBase(owner, "6768.hp."+splitdns.BrowserSuffix) != "" {
		t.Fatal("certificate authority escaped exact ownership")
	}
	if !validProtectedBindAddress("127.100.0.1:443", g.cfg.LoopbackCIDR) || validProtectedBindAddress("127.100.0.1:6767", g.cfg.LoopbackCIDR) || validAddressInCIDR(splitdns.BrowserGatewayIP, "127.100.0.0/16") {
		t.Fatal("gateway leaked into native allocation or lost custom-range protection")
	}
	if err := g.replaceAliases(t.Context(), owner, "owner", nil); err != nil {
		t.Fatal(err)
	}
	if g.certificateBase(owner, name) != "" || g.reserved.Names[name] != "owner" {
		t.Fatal("withdrawal retained authority or lost owner reservation")
	}
}

func TestBrowserAliasesRequireOwnedHTTPSAndPersistReservations(t *testing.T) {
	owner := &certificateConnection{identity: "owner"}
	other := &certificateConnection{identity: "other"}
	g := &guardServer{cfg: Config{StateDir: t.TempDir(), LoopbackCIDR: "127.100.0.0/16"}, reserved: reservations{IPs: map[string]string{}, Names: map[string]string{}}, leases: map[string]*guardedLease{"https": {hostname: "office.pprbt", ip: "127.100.1.2", port: 443, owner: owner}}, names: map[controlConn]map[string]string{}, certificates: map[controlConn]map[string]cachedCertificate{}}
	for _, bad := range []map[string]string{{"app.office.pprbt": "office.pprbt"}, {"app.pprbt": "missing.pprbt"}, {"app.mydev": "office.pprbt"}, {"office.pprbt": "office.pprbt"}} {
		if err := g.replaceAliases(context.Background(), owner, "owner", bad); err == nil {
			t.Fatalf("accepted invalid aliases %v", bad)
		}
	}
	if err := g.replaceAliases(context.Background(), other, "other", map[string]string{"app.pprbt": "office.pprbt"}); err == nil {
		t.Fatal("foreign connection borrowed HTTPS lease")
	}
	aliases := map[string]string{"app.pprbt": "office.pprbt"}
	if err := g.replaceAliases(context.Background(), owner, "owner", aliases); err != nil {
		t.Fatal(err)
	}
	aliases["app.pprbt"] = "changed.pprbt"
	if g.certificateBase(owner, "app.pprbt") != "office.pprbt" {
		t.Fatal("alias not owned or input map retained")
	}
	if _, err := g.handle(context.Background(), owner, "owner", request{Operation: "names", Names: map[string]string{"app.pprbt": "127.100.1.2", "office.pprbt": "127.100.1.2"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(g.cfg.StateDir, "reservations.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved reservations
	if err = json.Unmarshal(raw, &saved); err != nil || saved.Names["app.pprbt"] != "owner" {
		t.Fatal("alias reservation not durable", err)
	}

	for i := len(g.reserved.Names); i < 4096; i++ {
		g.reserved.Names[fmt.Sprintf("reserved%d.pprbt", i)] = "other"
	}
	if err := g.replaceAliases(context.Background(), owner, "owner", map[string]string{"app.pprbt": "office.pprbt"}); err != nil {
		t.Fatal("existing reservation rejected at global capacity", err)
	}
	if err := g.replaceAliases(context.Background(), owner, "owner", map[string]string{"new.pprbt": "office.pprbt"}); err == nil {
		t.Fatal("new alias exceeded global capacity")
	}
	g.reserved.Names["foreign.pprbt"] = "other"
	if err := g.replaceAliases(context.Background(), owner, "owner", map[string]string{"foreign.pprbt": "office.pprbt"}); err == nil {
		t.Fatal("foreign tombstone stolen")
	}

	g.leases["raw"] = &guardedLease{hostname: "office.pprbt", ip: "127.100.1.2", port: 3000, owner: owner}
	delete(g.leases, "https")
	if !g.removeUnservedNames(owner) || g.dnsView["app.pprbt"] != "" || g.dnsView["office.pprbt"] == "" {
		t.Fatal("HTTPS loss did not withdraw browser alias while preserving raw device DNS")
	}
	if g.certificateBase(owner, "app.pprbt") != "" {
		t.Fatal("HTTPS loss retained certificate authority")
	}
	if err := g.replaceAliases(context.Background(), owner, "owner", nil); err != nil {
		t.Fatal(err)
	}
	if g.certificateBase(owner, "app.pprbt") != "" || g.dnsView["app.pprbt"] != "" {
		t.Fatal("withdrawn alias remained usable")
	}
	if g.dnsView["office.pprbt"] == "" || g.reserved.Names["app.pprbt"] != "owner" {
		t.Fatal("base DNS or tombstone lost")
	}
}

func TestPortSubdomainRequiresExactOwnedAlias(t *testing.T) {
	owner := &certificateConnection{identity: "owner"}
	g := &guardServer{cfg: Config{StateDir: t.TempDir(), LoopbackCIDR: "127.100.0.0/16"}, reserved: reservations{IPs: map[string]string{}, Names: map[string]string{}}, leases: map[string]*guardedLease{"https": {hostname: "office.pprbt", ip: "127.100.1.2", port: 443, owner: owner}}, names: map[controlConn]map[string]string{}, certificates: map[controlConn]map[string]cachedCertificate{}}
	if err := g.replaceAliases(t.Context(), owner, "owner", map[string]string{"3000.office.pprbt": "office.pprbt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.handle(t.Context(), owner, "owner", request{Operation: "names", Names: map[string]string{"3000.office.pprbt": "127.100.1.2"}}); err != nil {
		t.Fatal(err)
	}
	if g.dnsView["3000.office.pprbt"] != "127.100.1.2" || g.certificateBase(owner, "3000.office.pprbt") != "office.pprbt" {
		t.Fatal("owned service alias was not published")
	}
	for _, name := range []string{"3001.office.pprbt", "0.office.pprbt", "65536.office.pprbt", "03000.office.pprbt", "3000.other.pprbt"} {
		if g.certificateBase(owner, name) != "" || g.dnsView[name] != "" {
			t.Fatalf("unregistered name %q borrowed certificate or DNS authority", name)
		}
	}
	if err := g.replaceAliases(t.Context(), owner, "owner", map[string]string{"3000.other.pprbt": "office.pprbt"}); err == nil {
		t.Fatal("alias crossed its device namespace")
	}
	if err := g.replaceAliases(t.Context(), owner, "owner", nil); err != nil {
		t.Fatal(err)
	}
	if g.dnsView["3000.office.pprbt"] != "" || g.certificateBase(owner, "3000.office.pprbt") != "" {
		t.Fatal("withdrawn service retained DNS or certificate authority")
	}
}
