package tailnet

import (
	"context"
	"errors"
	"testing"
	"time"

	"tailscale.com/types/key"
)

// F21-01: Paperboat unreachable for 10 minutes past the configuration's refresh hint. The daemon keeps its last
// verified peers (machine_network scopes without expiry), retries with the failure backoff instead of every
// 100 ms, and applies a revocation (a newer generation without the peer) as soon as it reconnects; a replayed
// lower generation stays refused.
func TestNetworkOutageKeepsPeersUntilARevocationArrives(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	cfg.Self.Role, cfg.Self.MachineID, cfg.Self.MachineGeneration = "machine", cfg.Self.EndpointID, 1
	a.options.Self = cfg.Self
	cfg.ExpiresAt = time.Now().Unix() + 2
	cfg.Peers[0].Scopes = []NetworkScope{{ResourceKind: "machine_network", ResourceID: "network_pair", ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: 443, ExpiresAt: 0}}
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal("until-revoked network scope rejected:", err)
	}
	time.Sleep(time.Until(time.Unix(cfg.ExpiresAt, 0)) + 50*time.Millisecond)

	// The outage: every refresh fails. Ten minutes of the real schedule, from the delays Run would wait.
	requests, elapsed := 0, time.Duration(0)
	for failures := 1; elapsed < 10*time.Minute; failures++ {
		requests++
		elapsed += a.nextRefresh(errors.New("paperboat unreachable"), failures, 0)
	}
	// The failure backoff is capped by the healthy cadence (24-36 s), so an outage costs no more than a healthy
	// engine: at most 10 min / 24 s plus the first backoff steps. The 100 ms floor would be ~6,000.
	if limit := int(10*time.Minute/(24*time.Second)) + 7; requests > limit {
		t.Fatalf("%d refresh requests in a 10-minute outage, want at most %d", requests, limit)
	}
	if _, err := a.Peer("machine_test"); err != nil {
		t.Fatal("the outage dropped a verified peer:", err)
	}
	if !a.OutOfSync() {
		t.Fatal("the outage is not reported out of sync")
	}

	// Reconnect: the server's newer configuration no longer has the peer (revoked during the outage).
	revoked := cfg
	revoked.Generation++
	revoked.IssuedAt, revoked.ExpiresAt = time.Now().Unix(), time.Now().Unix()+120
	revoked.Peers = nil
	if err := a.Apply(t.Context(), networkToken(t, signer, revoked)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Peer("machine_test"); !errors.Is(err, ErrAdmission) {
		t.Fatal("a revocation delivered after reconnect was not applied:", err)
	}
	if a.OutOfSync() {
		t.Fatal("still out of sync after a fresh configuration")
	}
	stale := cfg
	stale.IssuedAt, stale.ExpiresAt = time.Now().Unix(), time.Now().Unix()+120
	if err := a.Apply(context.Background(), networkToken(t, signer, stale)); !errors.Is(err, ErrStaleAuthority) {
		t.Fatalf("a lower generation after the revocation = %v, want refused as stale", err)
	}
}

// Only machine_network scopes may be until-revoked: a grant without expiry is refused.
func TestOnlyNetworkScopesMayLastUntilRevoked(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	cfg.Peers[0].Scopes[0].ExpiresAt = 0
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); !errors.Is(err, ErrAuthority) {
		t.Fatalf("a machine_access grant without expiry = %v, want refused", err)
	}
}

// F21-11: a host's offline session rights are machine_access accept scopes without expiry, at the deciding
// policy version; Allows answers for them past the refresh hint.
func TestHostSessionRightsLastUntilRevoked(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	cfg.Self.Role, cfg.Self.MachineID, cfg.Self.MachineGeneration = "machine", cfg.Self.EndpointID, 1
	a.options.Self = cfg.Self
	cfg.Peers[0].Identity = networkTestBinding("cli", "cli_peer", "fd7a:115c:a1e0::3", key.NewNode())
	cfg.ExpiresAt = time.Now().Unix() + 2
	cfg.Peers[0].Scopes = []NetworkScope{{ResourceKind: "machine_access", ResourceID: "host_test", ResourceGeneration: 7, Capability: "terminal", Direction: "accept", Port: 443, ExpiresAt: 0}}
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal("until-revoked session right rejected:", err)
	}
	time.Sleep(time.Until(time.Unix(cfg.ExpiresAt, 0)) + 50*time.Millisecond)
	if !a.Allows("cli_peer", "host_test", "terminal", "accept") {
		t.Fatal("an until-revoked session right ended at the refresh hint")
	}
	if a.Allows("cli_peer", "host_test", "exec", "accept") {
		t.Fatal("a capability that was not granted is allowed")
	}
}
