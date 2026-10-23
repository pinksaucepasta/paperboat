package policy

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Decision answers one access question.
type Decision struct {
	Allowed bool `json:"allowed"`
	// Reason is owner, rule, denied_by_rule, no_rule, port_not_allowed, private_access_off,
	// outside_token_scope or unknown_subject.
	Reason string `json:"reason"`
	// Rules are the JSON Pointers of the rules that decided ("/rules/2"): the deny rules for
	// denied_by_rule, otherwise the allow rules that match.
	Rules []string `json:"rules,omitempty"`
}

var allPorts = []PortRange{{From: 1, To: 65535}}

// Enforced is the grant list the server stores and checks: Grants without private_access
// when the team switch is off.
func (c Compiled) Enforced() []Grant {
	if c.AllowPrivateAccess {
		return c.Grants
	}
	out := make([]Grant, 0, len(c.Grants))
	for _, g := range c.Grants {
		if g.Action != ActionPrivateAccess {
			out = append(out, g)
		}
	}
	return out
}

// owns is the owner guardrail, which no rule can take away: a personal machine's owner may
// do everything on it in the personal workspace, and the owner of a preview or tunnel, or of
// the personal machine it runs on, may always inspect and replay it, and the team owner keeps
// the team ENV.
func (c Compiled) owns(account string, t Target, action string) bool {
	inspector := action == ActionInspect || action == ActionReplay
	switch t.Kind {
	case "machine":
		m, ok := c.x.byMachineID[t.ID]
		return ok && m.PersonalOwner == account && (!c.x.inv.Team || inspector)
	case "env":
		// The team owner always keeps the team ENV, as today (a vault needs a key holder).
		m, ok := c.x.byAccount[account]
		return ok && m.Role == "owner" && (action == ActionENVRead || action == ActionENVWrite)
	case "preview", "tunnel":
		r, ok := c.x.resourceIDs[t.Kind+":"+t.ID]
		if !ok || !inspector {
			return false
		}
		m := c.x.byMachineID[r.MachineID]
		return r.Owner == account || (m.PersonalOwner != "" && m.PersonalOwner == account)
	}
	return false
}

// matching returns the grants in gs that answer (account, t, action): the target's own, and
// for a preview or tunnel also the grant on the machine it runs on.
func (c Compiled) matching(gs []Grant, account string, t Target, action string) []Grant {
	var out []Grant
	if g, ok := findGrant(gs, account, t, action); ok {
		out = append(out, g)
	}
	if t.Kind == "preview" || t.Kind == "tunnel" {
		if r, ok := c.x.resourceIDs[t.Kind+":"+t.ID]; ok {
			if g, ok := findGrant(gs, account, Target{Kind: "machine", ID: r.MachineID}, action); ok {
				out = append(out, g)
			}
		}
	}
	return out
}

func ports(gs []Grant) []PortRange {
	var out []PortRange
	for _, g := range gs {
		out = append(out, g.Ports...)
	}
	return mergePorts(out)
}

func portsContain(ps []PortRange, port int) bool {
	for _, p := range ps {
		if p.contains(port) {
			return true
		}
	}
	return false
}

// Decide answers whether a member may perform action on t: owner guardrail, then an explicit
// deny, then an allow, else denied. port is used for private_access; 0 asks whether any port
// is allowed. Guardrails outside the policy (suspension, membership state, entitlement,
// on-device switches) are the server's.
func (c Compiled) Decide(account string, t Target, action string, port int) Decision {
	if _, ok := c.x.byAccount[account]; !ok {
		return Decision{Reason: "unknown_subject"}
	}
	if c.owns(account, t, action) {
		return Decision{Allowed: true, Reason: "owner"}
	}
	allows, denies := c.matching(c.Allows, account, t, action), c.matching(c.Denies, account, t, action)
	if len(denies) > 0 && (action != ActionPrivateAccess || (port != 0 && portsContain(ports(denies), port))) {
		return Decision{Reason: "denied_by_rule", Rules: rulePaths(denies)}
	}
	if len(allows) == 0 {
		return Decision{Reason: "no_rule"}
	}
	paths := rulePaths(allows)
	if action == ActionPrivateAccess {
		open := subtractPorts(ports(allows), ports(denies))
		switch {
		case !c.AllowPrivateAccess:
			return Decision{Reason: "private_access_off", Rules: paths}
		case len(open) == 0:
			return Decision{Reason: "denied_by_rule", Rules: rulePaths(denies)}
		case port != 0 && !portsContain(open, port):
			return Decision{Reason: "port_not_allowed", Rules: paths}
		}
	}
	return Decision{Allowed: true, Reason: "rule", Rules: paths}
}

// DecideToken answers for a request made with an API token: the token acts as its creator,
// and a scoped token (one in Inventory.Tokens) also needs its scope to cover the request.
func (c Compiled) DecideToken(token, creator string, t Target, action string, port int) Decision {
	d := c.Decide(creator, t, action, port)
	scope, scoped := c.Scopes[token]
	if !d.Allowed || !scoped {
		return d
	}
	in := c.matching(scope, "", t, action)
	if len(in) == 0 || (action == ActionPrivateAccess && port != 0 && !portsContain(ports(in), port)) {
		return Decision{Reason: "outside_token_scope"}
	}
	return d
}

func rulePaths(gs []Grant) []string {
	var idx []int
	for _, g := range gs {
		idx = append(idx, g.Rules...)
	}
	slices.Sort(idx)
	idx = slices.Compact(idx)
	out := make([]string, len(idx))
	for i, r := range idx {
		out[i] = pointer("/rules", r)
	}
	return out
}

// RunTests runs the file's tests against c. Each failure names the test entry.
func RunTests(d Document, c Compiled) Errors {
	e := &collector{}
	for i, t := range d.Tests {
		p := pointer("/tests", i)
		s, ok := c.x.user(t.Src)
		if !ok {
			e.add(pointer(p, "src"), "unknown_subject", "%q is not a member of this workspace (a test names one user:<email>)", t.Src)
		}
		tg, tok := c.x.target(t.Dst)
		if !tok {
			e.add(pointer(p, "dst"), "unknown_target", "%q is not a machine, preview, tunnel or env:team of this workspace", t.Dst)
		}
		if !ok || !tok {
			continue
		}
		for _, f := range []struct {
			field string
			list  []string
			want  bool
		}{{"allow", t.Allow, true}, {"deny", t.Deny, false}} {
			for j, a := range f.list {
				action, port, valid := testAction(a)
				if !valid {
					e.add(pointer(p, f.field, j), "unknown_action", "unknown action %q (use private_access:<port> for one port)", a)
					continue
				}
				got := c.Decide(s, tg, action, port)
				if got.Allowed != f.want {
					e.add(pointer(p, f.field, j), "test_failed", "expected %s to be %s %s on %s, but the policy says %s",
						t.Src, map[bool]string{true: "allowed", false: "denied"}[f.want], a, t.Dst, describe(got))
				}
			}
		}
	}
	return e.errs.sorted()
}

func testAction(a string) (string, int, bool) {
	name, port, hasPort := strings.Cut(a, ":")
	known := false
	for _, list := range [][]string{MachineActions, ENVActions} {
		for _, x := range list {
			known = known || x == name
		}
	}
	if !hasPort {
		return name, 0, known
	}
	n, err := strconv.Atoi(port)
	return name, n, known && name == ActionPrivateAccess && err == nil && n >= 1 && n <= 65535
}

func describe(d Decision) string {
	switch d.Reason {
	case "owner":
		return "allowed (owner)"
	case "rule":
		return "allowed by " + strings.Join(d.Rules, ", ")
	case "no_rule":
		return "denied (no rule matches)"
	case "port_not_allowed":
		return "denied (that port is not in " + strings.Join(d.Rules, ", ") + ")"
	case "denied_by_rule":
		return "denied by " + strings.Join(d.Rules, ", ")
	case "private_access_off":
		return "denied (allow_private_access is false)"
	}
	return fmt.Sprintf("denied (%s)", d.Reason)
}
