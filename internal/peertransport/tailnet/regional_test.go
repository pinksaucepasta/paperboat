package tailnet

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func regionalToken(t *testing.T, private ed25519.PrivateKey, candidates RegionalCandidates) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-regional-candidates+jwt", "kid": "network_test"})
	body, err := json.Marshal(candidates)
	if err != nil {
		t.Fatal(err)
	}
	signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(signed)))
}

func TestRegionalCandidatesSignedEndpointAndGenerationBinding(t *testing.T) {
	a, network, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, network)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	candidates := RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: network.Issuer, Audience: "paperboat-regional-candidates", AccountID: network.Self.AccountID, EndpointID: network.Self.EndpointID, AuthorizationGeneration: network.Generation, Generation: 1, IssuedAt: now, ExpiresAt: now + 60, Nodes: []RegionalNode{}}
	if err := a.ApplyRegionalCandidates(t.Context(), regionalToken(t, signer, candidates)); err != nil {
		t.Fatal(err)
	}
	candidates.EndpointID = "another_endpoint"
	if err := a.ApplyRegionalCandidates(t.Context(), regionalToken(t, signer, candidates)); !errors.Is(err, ErrRegionalAuthority) {
		t.Fatalf("cross-endpoint candidates error = %v", err)
	}
	candidates.EndpointID = network.Self.EndpointID
	candidates.AuthorizationGeneration++
	if err := a.ApplyRegionalCandidates(t.Context(), regionalToken(t, signer, candidates)); !errors.Is(err, ErrRegionalAuthority) {
		t.Fatalf("wrong authorization generation error = %v", err)
	}
}

func TestRegionalCandidatesEligibilityExpiryAndReducedRedundancy(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	drain := now.Add(20 * time.Second).Unix()
	node := func(id, region, domain, state string, used uint64) RegionalNode {
		return RegionalNode{NodeID: id, NodeGeneration: 1, ProcessEpoch: "epoch-1", Region: region, FailureDomain: domain,
			Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: id + ".example.test", EndpointQUICPort: 443,
			State: state, ObservedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(), CapacityLimit: 100, CapacityUsed: used, CapacityObservedAt: now.Unix()}
	}
	ready := node("ready", "hel", "hel-a", "ready", 20)
	excluded := node("excluded", "bom", "bom-a", "ready", 20)
	draining := node("draining", "hel", "hel-b", "ready", 20)
	draining.DrainDeadline = &drain
	stale := node("stale", "hel", "hel-c", "ready", 20)
	stale.ObservedAt = now.Add(-16 * time.Second).Unix()
	overloaded := node("overloaded", "hel", "hel-d", "ready", 90)
	candidates := RegionalCandidates{ExpiresAt: now.Add(time.Minute).Unix(), Nodes: []RegionalNode{excluded, draining, stale, overloaded, ready}}

	got, redundancy, err := candidates.Eligible("relay", "derp_quic", map[string]bool{"hel": true}, now)
	if err != nil || len(got) != 1 || got[0].NodeID != "ready" || redundancy != RedundancyReduced {
		t.Fatalf("eligible = %#v, redundancy = %q, err = %v", got, redundancy, err)
	}
	if _, _, err := candidates.Eligible("relay", "derp_quic", nil, now.Add(61*time.Second)); !errors.Is(err, ErrRegionalAuthority) {
		t.Fatalf("expired candidates error = %v", err)
	}
}

func TestRegionalCandidatesDifferentFailureDomainIsObservable(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	node := func(id, domain string) RegionalNode {
		return RegionalNode{NodeID: id, NodeGeneration: 1, ProcessEpoch: "epoch", Region: "hel", FailureDomain: domain, Roles: []string{"edge"}, Transports: []string{"http3"}, EndpointHost: id + ".example.test", EndpointQUICPort: 443, State: "ready", ObservedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(), CapacityLimit: 10, CapacityObservedAt: now.Unix()}
	}
	candidates := RegionalCandidates{ExpiresAt: now.Add(time.Minute).Unix(), Nodes: []RegionalNode{node("a", "rack-a"), node("b", "rack-b")}}
	got, redundancy, err := candidates.Eligible("edge", "http3", nil, now)
	if err != nil || len(got) != 2 || redundancy != RedundancyAvailable {
		t.Fatalf("eligible = %#v, redundancy = %q, err = %v", got, redundancy, err)
	}
}

func TestRegionalCandidateRefreshJitterIsBounded(t *testing.T) {
	for range 256 {
		delay := regionalRefreshDelay()
		if delay < 24*time.Second || delay > 36*time.Second {
			t.Fatalf("refresh delay = %s", delay)
		}
	}
}
