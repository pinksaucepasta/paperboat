//go:build linux || darwin

package machineguard

import (
	"errors"
	"maps"
	"testing"
)

type namesTestConn struct {
	controlConn
	id int
}

// Multi-account hosts (PLAN 3b, F23-01): publications are keyed by (uid, hostname), so two OS users both publish
// hp.<domain> for their own accounts' machines; a clash with another connection of the same user refuses only
// that name and keeps the rest (a clash degrades one item, never the whole reconcile).
func TestTwoOSUsersPublishTheSameMachineNameAndAClashDegradesOnlyThatName(t *testing.T) {
	state := t.TempDir()
	recordTestDefaultDomain(t, state)
	hp, dev, lab := "hp."+testBrowserDomain, "dev."+testBrowserDomain, "lab."+testBrowserDomain
	a, b, a2 := &namesTestConn{id: 1}, &namesTestConn{id: 2}, &namesTestConn{id: 3}
	g := &guardServer{
		cfg:         Config{StateDir: state, LoopbackCIDR: "127.100.0.0/16"},
		names:       map[controlConn]map[string]string{},
		connections: map[controlConn]string{a: "1000", b: "1001", a2: "1000"},
		leases: map[string]*guardedLease{
			"a-hp":   {hostname: hp, ip: "127.100.0.2", owner: a, uid: "1000"},
			"a-dev":  {hostname: dev, ip: "127.100.0.5", owner: a, uid: "1000"},
			"b-hp":   {hostname: hp, ip: "127.100.0.4", owner: b, uid: "1001"},
			"a2-hp":  {hostname: hp, ip: "127.100.0.6", owner: a2, uid: "1000"},
			"a2-lab": {hostname: lab, ip: "127.100.0.7", owner: a2, uid: "1000"},
		},
	}
	if _, err := g.handle(t.Context(), a, "1000", request{Operation: "names", Names: map[string]string{hp: "127.100.0.2", dev: "127.100.0.5"}}); err != nil {
		t.Fatalf("first user: %v", err)
	}
	if _, err := g.handle(t.Context(), b, "1001", request{Operation: "names", Names: map[string]string{hp: "127.100.0.4"}}); err != nil {
		t.Fatalf("a second OS user was refused the same machine name: %v", err)
	}
	_, err := g.handle(t.Context(), a2, "1000", request{Operation: "names", Names: map[string]string{hp: "127.100.0.6", lab: "127.100.0.7"}})
	if !errors.Is(err, ErrNamesRefused) {
		t.Fatalf("a same-user clash: err=%v, want ErrNamesRefused", err)
	}
	if got := g.names[a2]; len(got) != 1 || got[lab] != "127.100.0.7" {
		t.Fatalf("the clash must degrade only %s; published=%v", hp, got)
	}
	if g.names[a][hp] != "127.100.0.2" || g.names[b][hp] != "127.100.0.4" {
		t.Fatalf("the other publications changed: %v", g.names)
	}
}

// pbvm1, 2026-11-05: a second OS user paired into the SAME account went degraded ("Local machine access is
// unavailable", local_access_machine_names_failed): both daemons publish every machine of the account under the
// same names, and the guard refused the second user's whole names batch. Each uid gets its own address for the
// same (account, machine) claim and publishes the account's names without a clash.
func TestTwoOSUsersInTheSameAccountGetDistinctAddressesAndBothPublishTheirNames(t *testing.T) {
	state := t.TempDir()
	recordTestDefaultDomain(t, state)
	hp, lab := "hp."+testBrowserDomain, "lab."+testBrowserDomain
	a, b := &namesTestConn{id: 1}, &namesTestConn{id: 2}
	g := &guardServer{
		cfg:         Config{StateDir: state, LoopbackCIDR: "127.100.0.0/16"},
		reserved:    reservations{Names: map[string]string{}, Claims: map[string]claim{}},
		names:       map[controlConn]map[string]string{},
		connections: map[controlConn]string{a: "1000", b: "1001"},
		leases:      map[string]*guardedLease{},
	}
	g.claims = &claimBook{Claims: g.reserved.Claims}
	published := map[controlConn]map[string]string{a: {}, b: {}}
	for _, user := range []struct {
		conn controlConn
		uid  string
	}{{a, "1000"}, {b, "1001"}} {
		for machine, name := range map[string]string{"m-hp": hp, "m-lab": lab} {
			in, err := g.assignClaim(user.uid, request{Operation: "acquire", Account: "acct-1", Machine: machine, Hostname: name, IP: "127.100.0.9", Port: 22})
			if err != nil {
				t.Fatalf("uid %s, %s: %v", user.uid, machine, err)
			}
			g.leases[user.uid+machine] = &guardedLease{hostname: name, ip: in.IP, port: 22, owner: user.conn, uid: user.uid}
			published[user.conn][name] = in.IP
		}
	}
	for conn, names := range published {
		if _, err := g.handle(t.Context(), conn, g.connections[conn], request{Operation: "names", Names: names}); err != nil {
			t.Fatalf("uid %s: names refused: %v", g.connections[conn], err)
		}
	}
	addresses := map[string]bool{}
	for _, names := range published {
		for _, ip := range names {
			addresses[ip] = true
		}
	}
	if len(addresses) != 4 {
		t.Fatalf("two users × two machines need four distinct addresses: %v", published)
	}
	if !maps.Equal(g.names[a], published[a]) || !maps.Equal(g.names[b], published[b]) {
		t.Fatalf("published=%v, want %v", g.names, published)
	}
}
