package derpquic

import (
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	"testing"
	"time"

	"tailscale.com/types/key"
)

// A team member's CLI and the shared host (another account) name each other with standing machine_access scopes the
// server stamps with the grant expiry (F5-10). The relay pairs them per peer on the reciprocal scopes, with no
// machine_network scope; a different generation or capability pairs nothing, so a revoked share (generation bump) stops
// pairing on the next grant.
func TestPerPeerTeamMachineAccessScopesPairAcrossAccounts(t *testing.T) {
	now := time.Now()
	later := now.Add(time.Minute).Unix()
	member, host := KeyString(key.NewNode().Public()), KeyString(key.NewNode().Public())
	grant := func(account, endpoint, self, peer string, scopes ...Scope) Grant {
		return Grant{RelayGrant: peernetwork.RelayGrant{AccountID: account, EndpointID: endpoint, WireGuardPublicKey: self, ExpiresAt: later, Peers: []Peer{{WireGuardPublicKey: peer, Scopes: scopes}}}}.withIndex()
	}
	at := func(capability, direction string, generation uint64) Scope {
		s := scope("machine_access", "host_endpoint", capability, direction, later)
		s.ResourceGeneration = generation
		return s
	}
	h := grant("owner", "host_endpoint", host, member, at("exec", "accept", 7), at("managed_ssh", "accept", 7))
	if m := grant("member", "member_cli", member, host, at("exec", "dial", 7), at("managed_ssh", "dial", 7)); !allowed(m, h, now) || !allowed(h, m, now) || controlAdmitted(m, h) {
		t.Fatal("the member and the shared host are not paired for data, or are paired for relay control")
	}
	if m := grant("member", "member_cli", member, host, at("exec", "dial", 8)); allowed(m, h, now) {
		t.Fatal("a scope of another generation paired the member with the host")
	}
	if m := grant("member", "member_cli", member, host, at("terminal", "dial", 7)); allowed(m, h, now) {
		t.Fatal("a capability the host does not accept paired the member with the host")
	}
}

// The pair the server mints for a team member and the shared host (F5-10) forwards packets on a live relay: data both
// ways and relay control (the pair is data-authorized, so control passes although the accounts differ); a host that
// accepts other capabilities than the member dials gets nothing.
func TestTeamMemberAndSharedHostForwardThroughTheRelay(t *testing.T) {
	f := newFixture(t)
	_, address, _ := f.start("127.0.0.1:0")
	km, kh := key.NewNode().Public(), key.NewNode().Public()
	cm, ch := certificate(t), certificate(t)
	gm, gh := f.grant(km, cm, "member_cli"), f.grant(kh, ch, "shared_host")
	gh.AccountID = "host_owner"
	team := func(direction string, capabilities ...string) []Scope {
		var out []Scope
		for _, c := range capabilities {
			out = append(out, Scope{ResourceKind: "machine_access", ResourceID: "host_endpoint", ResourceGeneration: 7, Capability: c, Direction: direction, Port: 443, ExpiresAt: gm.ExpiresAt})
		}
		return out
	}
	gm.Peers = []Peer{{WireGuardPublicKey: gh.WireGuardPublicKey, Scopes: team("dial", "exec", "managed_ssh")}}
	gh.Peers = []Peer{{WireGuardPublicKey: gm.WireGuardPublicKey, Scopes: team("accept", "exec", "managed_ssh")}}
	m, h := f.client(address, cm, gm), f.client(address, ch, gh)
	connect(t, m)
	connect(t, h)
	request, reply := []byte("ssh to the shared host"), []byte("host answers the member")
	if err := m.Send(kh, request); err != nil {
		t.Fatal(err)
	}
	receive(t, h, km, request)
	if err := h.Send(km, reply); err != nil {
		t.Fatal(err)
	}
	receive(t, m, kh, reply)
	if err := m.SendControl(kh, []byte("relay control")); err != nil {
		t.Fatal(err)
	}
	receive(t, h, km, []byte("relay control"))

	other := f.grant(kh, ch, "shared_host_terminal_only")
	other.AccountID = "host_owner"
	other.Peers = []Peer{{WireGuardPublicKey: gm.WireGuardPublicKey, Scopes: team("accept", "terminal")}}
	if allowed(gm.withIndex(), other.withIndex(), time.Now()) {
		t.Fatal("a host accepting only terminal was paired with a member dialing exec and managed_ssh")
	}
}
