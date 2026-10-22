package policy

import "slices"

// Access is one effective permission in policy-file names, for explain answers and the
// access-change review.
type Access struct {
	Subject string   `json:"subject"` // "user:ana@example.test"
	Target  string   `json:"target"`  // "machine:db1"
	Action  string   `json:"action"`
	Ports   string   `json:"ports,omitempty"` // "1024-65535", "22,5432"
	Reason  string   `json:"reason"`          // owner or rule
	Rules   []string `json:"rules,omitempty"`
}

// Change is one access gained or lost by a policy change.
type Change struct {
	Change string `json:"change"` // gained or lost
	Access
}

func (c Compiled) access(g Grant) Access {
	a := Access{Subject: c.x.userLabel(g.Account), Target: c.x.targetLabel(g.Target), Action: g.Action, Reason: "rule", Rules: rulePaths([]Grant{g})}
	if g.Action == ActionPrivateAccess {
		a.Ports = portsLabel(g.Ports)
	}
	return a
}

func portsLabel(ps []PortRange) string {
	s := ""
	for i, p := range ps {
		if i > 0 {
			s += ","
		}
		s += p.String()
	}
	return s
}

// Changes lists the enforced access that differs between before and after: grants added,
// removed, or with different ports (shown as lost then gained). Owner access never changes
// with the policy and is not listed.
func Changes(before, after Compiled) []Change {
	var out []Change
	b, a := before.Enforced(), after.Enforced()
	i, j := 0, 0
	for i < len(b) || j < len(a) {
		switch {
		case j == len(a) || (i < len(b) && grantLess(b[i], a[j])):
			out = append(out, Change{Change: "lost", Access: before.access(b[i])})
			i++
		case i == len(b) || grantLess(a[j], b[i]):
			out = append(out, Change{Change: "gained", Access: after.access(a[j])})
			j++
		default:
			if !slices.Equal(b[i].Ports, a[j].Ports) {
				out = append(out, Change{Change: "lost", Access: before.access(b[i])}, Change{Change: "gained", Access: after.access(a[j])})
			}
			i, j = i+1, j+1
		}
	}
	return out
}

// WhoCan lists every member allowed action on t, with the reason.
func (c Compiled) WhoCan(t Target, action string) []Access {
	var out []Access
	for _, m := range c.x.inv.Members {
		d := c.Decide(m.AccountID, t, action, 0)
		if !d.Allowed {
			continue
		}
		a := Access{Subject: c.x.userLabel(m.AccountID), Target: c.x.targetLabel(t), Action: action, Reason: d.Reason, Rules: d.Rules}
		if action == ActionPrivateAccess && d.Reason == "rule" {
			a.Ports = portsLabel(subtractPorts(ports(c.matching(c.Allows, m.AccountID, t, action)), ports(c.matching(c.Denies, m.AccountID, t, action))))
		}
		out = append(out, a)
	}
	return out
}

// WhatCan lists everything a member may do: owner access on their own machines, then the
// enforced grants.
func (c Compiled) WhatCan(account string) []Access {
	var out []Access
	for _, m := range c.x.inv.Machines {
		t := Target{Kind: "machine", ID: m.ID}
		for _, action := range MachineActions {
			if c.owns(account, t, action) {
				out = append(out, Access{Subject: c.x.userLabel(account), Target: c.x.targetLabel(t), Action: action, Reason: "owner"})
			}
		}
	}
	for _, g := range c.Enforced() {
		if g.Account == account {
			out = append(out, c.access(g))
		}
	}
	return out
}

// User resolves "user:<email>" to an account ID in this compiled policy's workspace.
func (c Compiled) User(name string) (string, bool) { return c.x.user(name) }

// Target resolves "machine:<alias>", "preview:<name>", "tunnel:<name>" or "env:team".
func (c Compiled) Target(name string) (Target, bool) { return c.x.target(name) }
