package derpquic

import (
	"fmt"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	"runtime"
	"testing"
	"time"

	"tailscale.com/types/key"
)

func membershipGrant(account, endpoint string, expires int64, memberships ...Scope) Grant {
	g := Grant{RelayGrant: peernetwork.RelayGrant{AccountID: account, EndpointID: endpoint, WireGuardPublicKey: KeyString(key.NewNode().Public()), ExpiresAt: expires, Memberships: memberships}}
	return g.withIndex()
}

func scope(kind, id, capability, direction string, expires int64) Scope {
	return Scope{ResourceKind: kind, ResourceID: id, ResourceGeneration: 1, Capability: capability, Direction: direction, Port: 443, ExpiresAt: expires}
}

// A pair is forwarded when the two grants hold the same resource in opposite directions, as with per-peer
// scopes: the account mesh only within its account, an access grant across accounts, and a grant still in
// the per-peer shape pairs with one in the membership shape while the server moves between them.
func TestMembershipGrantsAuthorizeExactlyTheReciprocalPairs(t *testing.T) {
	now := time.Now()
	later := now.Add(time.Minute).Unix()
	mesh := func(account string) []Scope {
		return []Scope{scope("machine_network", "network_"+account, "connect", "dial", later), scope("machine_network", "network_"+account, "connect", "accept", later)}
	}
	a1 := membershipGrant("account_a", "a1", later, mesh("account_a")...)
	a2 := membershipGrant("account_a", "a2", later, mesh("account_a")...)
	b1 := membershipGrant("account_b", "b1", later, mesh("account_b")...)
	// account_b's machine shared with account_a's CLI through one access grant
	shared := membershipGrant("account_b", "b_machine", later, append(mesh("account_b"), scope("machine_access", "access_1", "terminal", "accept", later))...)
	viewer := membershipGrant("account_a", "a_cli", later, append(mesh("account_a"), scope("machine_access", "access_1", "terminal", "dial", later))...)
	outsider := membershipGrant("account_c", "c_cli", later, scope("machine_access", "access_2", "terminal", "dial", later))
	if !allowed(a1, a2, now) || !controlAdmitted(a1, a2) {
		t.Fatal("two endpoints of one account's mesh are not paired")
	}
	if allowed(a1, b1, now) || controlAdmitted(a1, b1) {
		t.Fatal("two accounts' meshes were paired")
	}
	if !allowed(viewer, shared, now) || allowed(outsider, shared, now) {
		t.Fatal("an access grant pairs the wrong endpoints")
	}
	// A share or a private route is one id held by its host (accept) and every viewer (dial): two viewers
	// hold the same membership in the same direction and must not be paired with each other (Lane B review).
	host := membershipGrant("account_b", "share_host", later, scope("machine_access", "share_1", "terminal", "accept", later))
	viewer1 := membershipGrant("account_c", "viewer_1", later, scope("machine_access", "share_1", "terminal", "dial", later))
	viewer2 := membershipGrant("account_d", "viewer_2", later, scope("machine_access", "share_1", "terminal", "dial", later))
	if !allowed(viewer1, host, now) || !allowed(viewer2, host, now) {
		t.Fatal("a viewer is not paired with the share's host")
	}
	if allowed(viewer1, viewer2, now) || allowedAdmitted(viewer1, viewer2) {
		t.Fatal("two viewers of one share were paired with each other")
	}
	// The per-peer shape on one side: the viewer names the machine with the same scope.
	perPeer := Grant{RelayGrant: peernetwork.RelayGrant{AccountID: "account_a", EndpointID: "a_cli_old", WireGuardPublicKey: KeyString(key.NewNode().Public()), ExpiresAt: later, Peers: []Peer{{WireGuardPublicKey: shared.WireGuardPublicKey, Scopes: []Scope{scope("machine_access", "access_1", "terminal", "dial", later)}}}}}.withIndex()
	if !allowed(perPeer, shared, now) || !allowed(shared, perPeer, now) {
		t.Fatal("a per-peer grant and a membership grant with reciprocal scopes are not paired")
	}
	// The pair ends with its membership.
	short := membershipGrant("account_a", "a_short", later, scope("machine_access", "access_1", "terminal", "dial", now.Add(5*time.Second).Unix()))
	if got := allowedPairExpiry(short, shared, now); got != now.Add(5*time.Second).Unix() {
		t.Fatalf("pair expiry %d, want the membership's", got)
	}
}

func TestMembershipShapeIsBoundedAndExclusive(t *testing.T) {
	later := time.Now().Add(time.Minute).Unix()
	m := scope("machine_network", "network_a", "connect", "dial", later)
	if !validMemberships(Grant{RelayGrant: peernetwork.RelayGrant{Memberships: []Scope{m}}}) {
		t.Fatal("a valid membership was refused")
	}
	if validMemberships(Grant{RelayGrant: peernetwork.RelayGrant{Memberships: []Scope{m, m}}}) {
		t.Fatal("a duplicate membership was accepted")
	}
	if validMemberships(Grant{RelayGrant: peernetwork.RelayGrant{Memberships: []Scope{m}, Peers: []Peer{{}}}}) {
		t.Fatal("memberships beside a per-peer list were accepted")
	}
	many := make([]Scope, MaxGrantMemberships+1)
	for i := range many {
		many[i] = scope("machine_access", fmt.Sprintf("access_%d", i), "terminal", "dial", later)
	}
	if validMemberships(Grant{RelayGrant: peernetwork.RelayGrant{Memberships: many}}) {
		t.Fatal("more than MaxGrantMemberships memberships were accepted")
	}
	if _, err := mergeGrants(Grant{RelayGrant: peernetwork.RelayGrant{Memberships: many[:MaxGrantMemberships]}}.withIndex(), Grant{RelayGrant: peernetwork.RelayGrant{Memberships: many[MaxGrantMemberships:]}}); err == nil {
		t.Fatal("merging past MaxGrantMemberships was accepted")
	}
}

// PLAN 3b budget: 10,000 endpoints of one account on one relay cost < 1 KiB per mesh grant (per-peer lists cost
// ~4.3 MiB per grant at 16,384 peers), < 1 µs for the first check of a pair with grants spread over memory,
// and < 100 ns per datagram after it (the connection remembers the pair until a grant changes). The test
// asserts what keeps those times, which wall clocks on shared machines cannot: the first check allocates
// nothing and a repeated datagram is answered by the pair memo. The benchmarks below report the times.
func TestTenThousandMeshEndpointsFitTheMembershipBudget(t *testing.T) {
	keys, grants, perGrant := tenThousandMeshGrants()
	if !raceEnabled && perGrant > 1024 {
		t.Fatalf("%d B per mesh grant, budget 1 KiB", perGrant)
	}
	i := 0
	if allocs := testing.AllocsPerRun(1000, func() {
		if !allowedAdmitted(grants[i%len(grants)], grants[(i*7919+1)%len(grants)]) {
			t.Fatal("two mesh endpoints are not paired")
		}
		i++
	}); allocs != 0 {
		t.Fatalf("first check of a pair allocates %v times", allocs)
	}
	s, p, q := pairedConnections(keys, grants)
	now := time.Now()
	if !s.authorizedLocked(p, q, now) {
		t.Fatal("a paired connection was refused")
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		if !s.authorizedLocked(p, q, now) {
			t.Fatal("a paired connection was refused")
		}
	}); allocs != 0 {
		t.Fatalf("a repeated datagram allocates %v times", allocs)
	}
	// The memo answers while both signed grants are the ones it was computed from: emptying the grant's
	// memberships under the same signed hash shows the pair is not checked again.
	memberships := q.grant.Memberships
	q.grant.Memberships = nil
	if !s.authorizedLocked(p, q, now) {
		t.Fatal("a repeated datagram checked the pair again instead of using the memo")
	}
	q.grant.Memberships = memberships
	q.grant = grants[2] // a renewed grant: the memo must not answer for it
	q.grant.AccountID = "account_b"
	if s.authorizedLocked(p, q, now) {
		t.Fatal("the pair memo answered for a changed grant")
	}
	// A refusal is remembered too: datagrams to an unpaired peer do not re-run the scope check under
	// Server.mu each time. Pairing the grant again under the same signed hash shows the memo answered.
	q.grant.AccountID = grants[2].AccountID
	if s.authorizedLocked(p, q, now) {
		t.Fatal("a repeated datagram to an unpaired peer checked the pair again instead of using the memo")
	}
	runtime.KeepAlive(grants)
}

func BenchmarkMembershipFirstCheck(b *testing.B) {
	_, grants, _ := tenThousandMeshGrants()
	b.ResetTimer()
	for i := range b.N {
		if !allowedAdmitted(grants[i%len(grants)], grants[(i*7919+1)%len(grants)]) {
			b.Fatal("two mesh endpoints are not paired")
		}
	}
}

func BenchmarkMembershipRepeatedDatagram(b *testing.B) {
	keys, grants, _ := tenThousandMeshGrants()
	s, p, q := pairedConnections(keys, grants)
	now := time.Now()
	b.ResetTimer()
	for range b.N {
		if !s.authorizedLocked(p, q, now) {
			b.Fatal("a paired connection was refused")
		}
	}
}

// tenThousandMeshGrants returns one account's 10,000 mesh grants and their bytes per grant. TotalAlloc only
// grows, so it is an upper bound (it also counts the construction's temporaries).
func tenThousandMeshGrants() ([]key.NodePublic, []Grant, uint64) {
	const endpoints = 10_000
	later := time.Now().Add(time.Minute).Unix()
	keys := make([]key.NodePublic, endpoints)
	for i := range keys {
		keys[i] = key.NewNode().Public()
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	grants := make([]Grant, endpoints)
	for i := range grants {
		grants[i] = Grant{RelayGrant: peernetwork.RelayGrant{AccountID: "account_a", EndpointID: fmt.Sprintf("endpoint_%05d", i), WireGuardPublicKey: KeyString(keys[i]), ExpiresAt: later, Memberships: []Scope{
			scope("machine_network", "network_account_a", "connect", "dial", later), scope("machine_network", "network_account_a", "connect", "accept", later),
		}}}.withIndex()
		grants[i].signedHash[0], grants[i].signedHash[1] = byte(i), byte(i>>8)
	}
	runtime.ReadMemStats(&after)
	return keys, grants, (after.TotalAlloc - before.TotalAlloc) / endpoints
}

func pairedConnections(keys []key.NodePublic, grants []Grant) (*Server, *peerConn, *peerConn) {
	s := &Server{peers: map[key.NodePublic]*peerConn{}}
	p, q := &peerConn{key: keys[0], grant: grants[0]}, &peerConn{key: keys[1], grant: grants[1]}
	s.peers[p.key], s.peers[q.key] = p, q
	return s, p, q
}
