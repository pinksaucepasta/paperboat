//go:build linux || darwin || windows

package machineguard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

func TestBrowserAliasesRequireOwnedHTTPSGatewayAndCanonicalShape(t *testing.T) {
	owner := &testControlConn{identity: "owner"}
	other := &testControlConn{identity: "other"}
	g := &guardServer{
		cfg:      Config{StateDir: t.TempDir(), LoopbackCIDR: "127.100.0.0/16"},
		reserved: reservations{IPs: map[string]string{}, Names: map[string]string{}},
		leases:   map[string]*guardedLease{"gateway": {hostname: splitdns.BrowserGatewayHostname, ip: splitdns.BrowserGatewayIP, port: 443, owner: owner}},
		aliases:  map[controlConn]map[string]string{},
		names:    map[controlConn]map[string]string{},
	}
	name := "6767.hp." + splitdns.BrowserSuffix
	aliases := map[string]string{name: splitdns.BrowserGatewayHostname}
	if err := g.replaceAliases(t.Context(), other, "other", aliases); err == nil {
		t.Fatal("foreign connection borrowed gateway")
	}
	for _, bad := range []string{
		"app.office.pprbt",
		"6767.gateway." + splitdns.BrowserSuffix,
		"06767.hp." + splitdns.BrowserSuffix,
		"0.hp." + splitdns.BrowserSuffix,
		"65536.hp." + splitdns.BrowserSuffix,
		"6767.other.extra." + splitdns.BrowserSuffix,
		"6767.HP." + splitdns.BrowserSuffix,
	} {
		if err := g.replaceAliases(t.Context(), owner, "owner", map[string]string{bad: splitdns.BrowserGatewayHostname}); err == nil {
			t.Fatalf("accepted noncanonical browser alias %q", bad)
		}
	}
	if err := g.replaceAliases(t.Context(), owner, "owner", aliases); err != nil {
		t.Fatal(err)
	}
	aliases[name] = "gateway.example.com"
	if g.aliases[owner][name] != splitdns.BrowserGatewayHostname || !g.leaseServesName(owner, g.leases["gateway"], name) || g.leaseServesName(other, g.leases["gateway"], name) {
		t.Fatal("alias ownership or copy semantics changed")
	}
	if saved, err := os.ReadFile(filepath.Join(g.cfg.StateDir, "reservations.json")); err != nil {
		t.Fatal(err)
	} else {
		var journal reservations
		if err = json.Unmarshal(saved, &journal); err != nil || journal.Names[name] != "owner" {
			t.Fatalf("browser alias reservation not durable: %+v %v", journal, err)
		}
	}
	if err := g.replaceAliases(t.Context(), owner, "owner", nil); err != nil {
		t.Fatal(err)
	}
	if len(g.aliases[owner]) != 0 || g.reserved.Names[name] != "owner" {
		t.Fatal("withdrawal retained active alias or lost owner reservation")
	}
}

func TestBrowserAliasesRequireOwnedHTTPSListenerAndReservationLimits(t *testing.T) {
	owner := &testControlConn{identity: "owner"}
	gateway := &guardedLease{hostname: splitdns.BrowserGatewayHostname, ip: splitdns.BrowserGatewayIP, port: 443, owner: owner}
	g := &guardServer{
		cfg:      Config{StateDir: t.TempDir(), LoopbackCIDR: "127.100.0.0/16"},
		reserved: reservations{IPs: map[string]string{}, Names: map[string]string{}},
		leases:   map[string]*guardedLease{"gateway": gateway},
		aliases:  map[controlConn]map[string]string{},
		names:    map[controlConn]map[string]string{},
	}
	valid := map[string]string{"3000.hp." + splitdns.BrowserSuffix: splitdns.BrowserGatewayHostname}
	if err := g.replaceAliases(context.Background(), owner, "owner", valid); err != nil {
		t.Fatal(err)
	}
	delete(g.leases, "gateway")
	if err := g.replaceAliases(context.Background(), owner, "owner", valid); err == nil {
		t.Fatal("alias survived loss of its HTTPS listener")
	}
	g.leases["gateway"] = gateway
	for i := len(g.reserved.Names); i < 4096; i++ {
		g.reserved.Names[fmt.Sprintf("reserved%d.local.pprbt.dev", i)] = "other"
	}
	if err := g.replaceAliases(context.Background(), owner, "owner", valid); err != nil {
		t.Fatalf("existing reservation rejected at global capacity: %v", err)
	}
	if err := g.replaceAliases(context.Background(), owner, "owner", map[string]string{"3001.hp." + splitdns.BrowserSuffix: splitdns.BrowserGatewayHostname}); err == nil {
		t.Fatal("new alias exceeded global capacity")
	}
	g.reserved.Names["3001.hp."+splitdns.BrowserSuffix] = "foreign"
	if err := g.replaceAliases(context.Background(), owner, "owner", map[string]string{"3001.hp." + splitdns.BrowserSuffix: splitdns.BrowserGatewayHostname}); err == nil {
		t.Fatal("foreign tombstone stolen")
	}
}

func TestAliasHostShapeUsesPublicNamespace(t *testing.T) {
	for _, host := range []string{"80.hp." + splitdns.BrowserSuffix, "65535.alias-1." + splitdns.BrowserSuffix} {
		if !validSubdomainName(host) {
			t.Fatalf("valid public browser alias rejected: %s", host)
		}
	}
	for _, host := range []string{
		"0.hp." + splitdns.BrowserSuffix,
		"080.hp." + splitdns.BrowserSuffix,
		"65536.hp." + splitdns.BrowserSuffix,
		"80.gateway." + splitdns.BrowserSuffix,
		"80.hp.extra." + splitdns.BrowserSuffix,
		"80.hp.pprbt",
	} {
		if validSubdomainName(host) {
			t.Fatalf("invalid public browser alias accepted: %s", host)
		}
	}
}

func TestMachineProxyPatternRequiresOwnedNumericRouteAndWithdraws(t *testing.T) {
	owner := &testControlConn{identity: "owner"}
	base := "homelab." + splitdns.BrowserSuffix
	numeric := "80." + base
	pattern := "*." + base
	gateway := &guardedLease{hostname: splitdns.BrowserGatewayHostname, ip: splitdns.BrowserGatewayIP, port: 443, owner: owner, uid: "owner"}
	g := &guardServer{cfg: Config{StateDir: t.TempDir()}, reserved: reservations{IPs: map[string]string{}, Names: map[string]string{}}, leases: map[string]*guardedLease{"gateway": gateway}, aliases: map[controlConn]map[string]string{}, names: map[controlConn]map[string]string{}, connections: map[controlConn]string{owner: "owner"}}
	for _, bad := range []map[string]string{{pattern: numeric}, {pattern: "80.other." + splitdns.BrowserSuffix, numeric: splitdns.BrowserGatewayHostname}, {"*.*." + base: numeric, numeric: splitdns.BrowserGatewayHostname}} {
		if err := g.replaceAliases(t.Context(), owner, "owner", bad); err == nil {
			t.Fatal("invalid wildcard authorized")
		}
	}
	aliases := map[string]string{pattern: numeric, numeric: splitdns.BrowserGatewayHostname, "bob." + base: ""}
	if err := g.replaceAliases(t.Context(), owner, "owner", aliases); err != nil {
		t.Fatal(err)
	}
	for _, app := range []string{"first", "second", "third"} {
		name := app + "." + base
		if !g.leaseServesName(owner, gateway, name) || !g.certificateRequestAuthorizedDomain(owner, "owner", name, "homelab", splitdns.BrowserSuffix) {
			t.Fatalf("app denied: %s", name)
		}
	}
	ca, err := splitdns.LoadOrCreateConstrainedCA(t.TempDir(), splitdns.BrowserSuffix)
	if err != nil {
		t.Fatal(err)
	}
	g.ca = ca
	var first *CertificateBundle
	for _, app := range []string{"first", "second", "third"} {
		bundle, err := g.certificate(t.Context(), owner, "owner", app+"."+base)
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = bundle
		} else if !bytes.Equal(first.CertificatePEM, bundle.CertificatePEM) {
			t.Fatal("per-app leaf issuance")
		}
	}
	if len(g.certificates[owner]) != 1 {
		t.Fatal("per-app certificate cache growth")
	}
	if len(g.reserved.Names) != 3 {
		t.Fatal("per-app reservations")
	}
	for _, name := range []string{"bob." + base, "81." + base, "080." + base, "65536." + base, "foo.bar." + base, "first.other." + splitdns.BrowserSuffix} {
		if g.leaseServesName(owner, gateway, name) || g.certificateRequestAuthorizedDomain(owner, "owner", name, "homelab", splitdns.BrowserSuffix) {
			t.Fatalf("invalid fallback: %s", name)
		}
	}
	if err := g.replaceAliases(t.Context(), owner, "owner", nil); err != nil {
		t.Fatal(err)
	}
	if g.leaseServesName(owner, gateway, "third."+base) || g.certificateRequestAuthorizedDomain(owner, "owner", "third."+base, "homelab", splitdns.BrowserSuffix) {
		t.Fatal("withdrawal retained authorization")
	}
	if _, err := g.certificate(t.Context(), owner, "owner", "third."+base); err == nil {
		t.Fatal("withdrawn cached certificate returned")
	}
	g.reserved.Names["foreign."+base] = "other"
	if err := g.replaceAliases(t.Context(), owner, "owner", aliases); err == nil {
		t.Fatal("foreign reservation crossed")
	}
}
