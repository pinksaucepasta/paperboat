//go:build linux || darwin || windows

package deviceguard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

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
