package peernetwork

import (
	"strings"
	"testing"
	"time"
)

func TestRegionalCandidatesSigningContract(t *testing.T) {
	now := time.Unix(1800000000, 0)
	valid := RegionalCandidates{Schema: RegionalCandidatesSchema, Issuer: "https://api.example.test", Audience: RegionalCandidatesAudience, AccountID: "account", EndpointID: "endpoint", AuthorizationGeneration: 1, Generation: 1, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix()}
	for _, tc := range []struct {
		name   string
		change func(*RegionalCandidates)
		valid  bool
	}{
		{"empty registry", func(c *RegionalCandidates) {}, true},
		{"self hosted pin", func(c *RegionalCandidates) {
			c.Nodes = []RegionalNode{{Hosting: HostingSelfHosted, TLSSPKISHA256: strings.Repeat("A", 43)}}
		}, true},
		{"bad pin", func(c *RegionalCandidates) { c.Nodes = []RegionalNode{{TLSSPKISHA256: "bad"}} }, false},
		{"unknown hosting", func(c *RegionalCandidates) { c.Nodes = []RegionalNode{{Hosting: "unknown"}} }, false},
		{"too many nodes", func(c *RegionalCandidates) { c.Nodes = make([]RegionalNode, MaxRegionalCandidates+1) }, false},
		{"unbounded generation", func(c *RegionalCandidates) { c.Generation = MaxSafeInteger + 1 }, false},
		{"no authorization", func(c *RegionalCandidates) { c.AuthorizationGeneration = 0 }, false},
		{"no issue time", func(c *RegionalCandidates) { c.IssuedAt = 0 }, false},
		{"future", func(c *RegionalCandidates) { c.IssuedAt = now.Add(2 * time.Minute).Unix() }, false},
		{"reversed lifetime", func(c *RegionalCandidates) { c.ExpiresAt = c.IssuedAt }, false},
		{"overlong lifetime", func(c *RegionalCandidates) { c.ExpiresAt = now.Add(24 * time.Hour).Unix() }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := valid
			tc.change(&c)
			if err := c.Check(now); (err == nil) != tc.valid {
				t.Fatalf("Check = %v, valid = %v", err, tc.valid)
			}
		})
	}
}
