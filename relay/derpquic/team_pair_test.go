package derpquic

import (
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
		return Grant{AccountID: account, EndpointID: endpoint, WireGuardPublicKey: self, ExpiresAt: later, Peers: []Peer{{WireGuardPublicKey: peer, Scopes: scopes}}}.withIndex()
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
