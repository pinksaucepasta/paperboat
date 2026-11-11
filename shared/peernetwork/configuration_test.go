package peernetwork

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func testBinding(endpoint, role string, n byte) Binding {
	b := Binding{AccountID: "account", EndpointID: endpoint, Role: role, EndpointGeneration: 1, KeyGeneration: 1, WireGuardPublicKey: testKey(n), DiscoPublicKey: testKey(n + 1),
		QUICCertificateFingerprint: strings.Repeat("ab", 32), QUICPublicKey: testKey(n + 2), VirtualAddress: fmt.Sprintf("fd7a:115c:a1e0::%x", n)}
	if role == RoleMachine {
		b.MachineID, b.MachineGeneration = endpoint, 1
	}
	return b
}

func validTestConfiguration(now time.Time) Configuration {
	scope := Scope{ResourceKind: "machine_access", ResourceID: "access_1", ResourceGeneration: 1, Capability: "terminal", Direction: "dial", Port: NetworkPort, ExpiresAt: now.Add(time.Minute).Unix()}
	standing := Scope{ResourceKind: "machine_access", ResourceID: "host", ResourceGeneration: 7, Capability: "exec", Direction: "dial", Port: NetworkPort}
	return Configuration{Version: 1, Issuer: "https://api.example.test", Audience: ConfigurationAudience, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(), Generation: 1,
		Self: testBinding("cli", RoleCLI, 10), Peers: []Peer{{Identity: testBinding("host", RoleMachine, 20), Scopes: []Scope{scope, standing}, Endpoints: []string{"203.0.113.7:41641"}}}}
}

func TestConfigurationCheckRefusesEachBrokenRule(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	if err := validTestConfiguration(now).Check(now); err != nil {
		t.Fatalf("valid configuration refused: %v", err)
	}
	for name, breakIt := range map[string]func(*Configuration){
		"audience":        func(c *Configuration) { c.Audience = RelayGrantAudience },
		"generation":      func(c *Configuration) { c.Generation = MaxSafeInteger + 1 },
		"future":          func(c *Configuration) { c.IssuedAt = now.Add(2 * MaxClockSkew).Unix(); c.ExpiresAt = c.IssuedAt + 60 },
		"self disco":      func(c *Configuration) { c.Self.DiscoPublicKey = "" },
		"peer address":    func(c *Configuration) { c.Peers[0].Identity.VirtualAddress = c.Self.VirtualAddress },
		"cli peer of cli": func(c *Configuration) { c.Peers[0].Identity = testBinding("cli2", RoleCLI, 30) },
		"peer endpoint":   func(c *Configuration) { c.Peers[0].Endpoints = []string{"127.0.0.1:1"} },
		"scope direction": func(c *Configuration) { c.Peers[0].Scopes[0].Direction = "accept" },
		"scope port":      func(c *Configuration) { c.Peers[0].Scopes[0].Port = 80 },
		"scope id":        func(c *Configuration) { c.Peers[0].Scopes[0].ResourceID = "bad id" },
		"standing timed":  func(c *Configuration) { c.Peers[0].Scopes[0].ExpiresAt = 0 },
		"duplicate scope": func(c *Configuration) { c.Peers[0].Scopes = append(c.Peers[0].Scopes, c.Peers[0].Scopes[0]) },
		"foreign mesh scope": func(c *Configuration) {
			c.Peers[0].Identity.AccountID = "other"
			c.Peers[0].Scopes[0].ResourceKind = "machine_network"
			c.Peers[0].Scopes[0].Capability = "connect"
		},
	} {
		c := validTestConfiguration(now)
		breakIt(&c)
		if err := c.Check(now); !errors.Is(err, ErrInvalidDocument) {
			t.Errorf("%s: Check = %v, want ErrInvalidDocument", name, err)
		}
	}
}

// An unknown scope authorizes nothing and never fails the configuration; a peer left with no scope is left out.
func TestConfigurationReceiversDropUnknownScopes(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := validTestConfiguration(now)
	unknown := c.Peers[0].Scopes[0]
	unknown.Capability = "future_capability"
	c.Peers[0].Scopes = append(c.Peers[0].Scopes, unknown)
	other := testBinding("host2", RoleMachine, 40)
	c.Peers = append(c.Peers, Peer{Identity: other, Scopes: []Scope{unknown}})
	c.DropUnknownScopes()
	if len(c.Peers) != 1 || len(c.Peers[0].Scopes) != 2 || c.Check(now) != nil {
		t.Fatalf("peers after dropping unknown scopes: %+v", c.Peers)
	}
}
