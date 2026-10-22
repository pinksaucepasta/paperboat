package policy

import (
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Grant is one compiled permission: an account may perform an action on a target. Rules
// lists the indexes of the rules that give (or, in Compiled.Denies, take away) it. Ports is
// set only for private_access.
type Grant struct {
	Account string      `json:"account"`
	Target  Target      `json:"target"`
	Action  string      `json:"action"`
	Ports   []PortRange `json:"ports,omitempty"`
	Rules   []int       `json:"rules"`
}

// Compiled is a validated policy expanded against one inventory. Every list is sorted and
// holds at most one entry per account (or token), target and action.
type Compiled struct {
	// Allows and Denies are what the allow and deny rules say, before one is taken from the
	// other; explain uses them to name the deciding rule.
	Allows []Grant
	Denies []Grant
	// Grants is Allows minus Denies: the access the policy gives.
	Grants             []Grant
	AllowPrivateAccess bool
	// Scopes holds each scoped API token's scope, keyed by token ID, with Account empty.
	// A scope entry whose names no longer resolve matches nothing.
	Scopes map[string][]Grant
	x      *index
	tags   map[string][]string
	groups map[string][]string
	rows   []Row
}

// Validate parses, checks against the inventory and limits, compiles, and runs the file's
// tests. A policy may be saved only when Errors is empty.
func Validate(src []byte, inv Inventory, limits Limits) (Document, Compiled, Errors) {
	if limits.MaxBytes > 0 && len(src) > limits.MaxBytes {
		return Document{}, Compiled{}, Errors{{Code: "too_large", Message: "the policy file is larger than the limit of " + bytesLabel(limits.MaxBytes)}}
	}
	d, errs := Parse(src)
	if len(errs) > 0 {
		return d, Compiled{}, errs
	}
	c, errs := Compile(d, inv, limits)
	if len(errs) > 0 {
		return d, c, errs
	}
	return d, c, RunTests(d, c)
}

// Compile checks every name in d against inv and the limits, then expands the rules into
// grants and the inventory's token scopes. It does not run the file's tests.
func Compile(d Document, inv Inventory, limits Limits) (Compiled, Errors) {
	c := &collector{}
	x := newIndex(inv)
	checkLimits(c, d, limits)
	groups := compileGroups(c, d, x)
	tags := compileTags(c, d, x)
	if d.AllowPrivateAccess != nil && !inv.Team {
		c.add("/allow_private_access", "team_only", "allow_private_access is a team setting; in a personal workspace it is always on")
	}
	allow, deny := newBuilder(), newBuilder()
	for i, r := range d.Rules {
		compileRule(c, allow, deny, x, groups, tags, pointer("/rules", i), i, r)
	}
	if len(c.errs) > 0 {
		return Compiled{x: x}, c.errs.sorted()
	}
	var rows []Row
	for i, r := range d.Rules {
		rows = append(rows, symbolicRows(x, i, r)...)
	}
	sortRows(rows)
	out := Compiled{Allows: allow.list(), Denies: deny.list(), x: x, tags: tags, groups: groups, rows: rows,
		AllowPrivateAccess: !inv.Team || (d.AllowPrivateAccess != nil && *d.AllowPrivateAccess)}
	out.Grants = subtract(out.Allows, out.Denies)
	out.Scopes = map[string][]Grant{}
	for _, t := range inv.Tokens {
		out.Scopes[t.ID], _ = out.compileScope(t.Scope)
	}
	return out, nil
}

// ValidateScope checks an API token scope against this policy's workspace; a token is
// created or changed only when it returns no errors. Paths are relative to the scope list.
func (c Compiled) ValidateScope(scope []ScopeEntry) Errors {
	_, errs := c.compileScope(scope)
	return errs
}

func (c Compiled) compileScope(scope []ScopeEntry) ([]Grant, Errors) {
	e := &collector{}
	b := newBuilder()
	if len(scope) == 0 {
		e.add("", "empty", "a scope lists at least one {dst, allow} entry; leave the scope out for full access")
	}
	for i, s := range scope {
		p := pointer("", i)
		before := len(e.errs)
		if len(s.Dst) == 0 || len(s.Allow) == 0 {
			e.add(p, "empty", "a scope entry needs dst and allow")
		}
		family, targets := ruleTargets(e, c.x, c.tags, pointer(p, "dst"), s.Dst)
		actions := ruleActions(e, family, pointer(p, "allow"), s.Allow)
		ports := rulePorts(e, p, actions, s.Ports, false)
		if len(e.errs) > before {
			continue
		}
		for _, t := range targets {
			for _, a := range actions {
				b.add("", t, a, ports, i)
			}
		}
	}
	return b.list(), e.errs.sorted()
}

func bytesLabel(n int) string {
	if n%1024 == 0 {
		return strconv.Itoa(n/1024) + " KiB"
	}
	return strconv.Itoa(n) + " bytes"
}

func checkLimits(c *collector, d Document, l Limits) {
	over := func(path string, n, max int, what string) {
		if max > 0 && n > max {
			c.add(path, "limit", "%d %s; the limit is %d", n, what, max)
		}
	}
	over("/rules", len(d.Rules), l.MaxRules, "rules")
	over("/tags", len(d.Tags), l.MaxTags, "tags")
	over("/groups", len(d.Groups), l.MaxGroups, "groups")
	over("/tests", len(d.Tests), l.MaxTests, "tests")
	for name, entries := range d.Groups {
		over(pointer("/groups", name), len(entries), l.MaxEntriesPerList, "entries")
	}
	for name, entries := range d.Tags {
		over(pointer("/tags", name), len(entries), l.MaxEntriesPerList, "entries")
	}
	for i, r := range d.Rules {
		over(pointer("/rules", i, "src"), len(r.Src), l.MaxEntriesPerList, "entries")
		over(pointer("/rules", i, "dst"), len(r.Dst), l.MaxEntriesPerList, "entries")
	}
}

func compileGroups(c *collector, d Document, x *index) map[string][]string {
	out := map[string][]string{}
	for name, entries := range d.Groups {
		p := pointer("/groups", name)
		g, ok := strings.CutPrefix(name, "group:")
		if !ok || !ValidName(g) {
			c.add(p, "invalid_name", "a group is named group:<name> (lowercase letters, digits, '.', '_', '-')")
			continue
		}
		var accounts []string
		for i, e := range entries {
			if a, ok := resolveUser(c, pointer(p, i), x, e, "a group holds user:<email> entries"); ok {
				accounts = append(accounts, a)
			}
		}
		out[name] = accounts
	}
	return out
}

func compileTags(c *collector, d Document, x *index) map[string][]string {
	out := map[string][]string{}
	for name, entries := range d.Tags {
		p := pointer("/tags", name)
		t, ok := strings.CutPrefix(name, "tag:")
		if !ok || !ValidName(t) {
			c.add(p, "invalid_name", "a tag is named tag:<name> (lowercase letters, digits, '.', '_', '-')")
			continue
		}
		var machines []string
		for i, e := range entries {
			ep := pointer(p, i)
			alias, ok := strings.CutPrefix(e, "machine:")
			if !ok || alias == "*" {
				c.add(ep, "not_allowed_in_tag", "a tag holds machine:<alias> entries, not %q", e)
				continue
			}
			if code := machineError(c, ep, x, alias); code != "" {
				continue
			}
			m, _ := x.machine(alias)
			machines = append(machines, m.ID)
		}
		out[name] = machines
	}
	return out
}

func resolveUser(c *collector, path string, x *index, e, what string) (string, bool) {
	if !strings.HasPrefix(e, "user:") {
		c.add(path, "invalid_subject", "%s, not %q", what, e)
		return "", false
	}
	a, ok := x.user(e)
	if !ok {
		c.add(path, "unknown_user", "%q is not a member of this workspace", strings.TrimPrefix(e, "user:"))
	}
	return a, ok
}

type grantKey struct {
	account string
	target  Target
	action  string
}

type builder struct{ grants map[grantKey]*Grant }

func newBuilder() *builder { return &builder{grants: map[grantKey]*Grant{}} }

func (b *builder) add(account string, t Target, action string, ports []PortRange, rule int) {
	k := grantKey{account, t, action}
	g, ok := b.grants[k]
	if !ok {
		g = &Grant{Account: account, Target: t, Action: action}
		b.grants[k] = g
	}
	if !slices.Contains(g.Rules, rule) {
		g.Rules = append(g.Rules, rule)
		slices.Sort(g.Rules)
	}
	if action == ActionPrivateAccess {
		g.Ports = mergePorts(append(g.Ports, ports...))
	}
}

func (b *builder) list() []Grant {
	out := make([]Grant, 0, len(b.grants))
	for _, g := range b.grants {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return grantLess(out[i], out[j]) })
	return out
}

func grantLess(a, b Grant) bool {
	ka := [...]string{a.Account, a.Target.Kind, a.Target.ID, a.Action}
	kb := [...]string{b.Account, b.Target.Kind, b.Target.ID, b.Action}
	return slices.Compare(ka[:], kb[:]) < 0
}

func findGrant(gs []Grant, account string, t Target, action string) (Grant, bool) {
	want := Grant{Account: account, Target: t, Action: action}
	i := sort.Search(len(gs), func(i int) bool { return !grantLess(gs[i], want) })
	if i < len(gs) && gs[i].Account == account && gs[i].Target == t && gs[i].Action == action {
		return gs[i], true
	}
	return Grant{}, false
}

// subtract removes denied access from allowed access: a whole grant, or for private_access
// the denied ports.
func subtract(allows, denies []Grant) []Grant {
	out := make([]Grant, 0, len(allows))
	for _, g := range allows {
		d, denied := findGrant(denies, g.Account, g.Target, g.Action)
		if !denied {
			out = append(out, g)
			continue
		}
		if g.Action != ActionPrivateAccess {
			continue
		}
		g.Ports = subtractPorts(g.Ports, d.Ports)
		if len(g.Ports) > 0 {
			out = append(out, g)
		}
	}
	return out
}

// mergePorts sorts and joins overlapping or adjacent ranges.
func mergePorts(ps []PortRange) []PortRange {
	if len(ps) == 0 {
		return nil
	}
	ps = slices.Clone(ps)
	sort.Slice(ps, func(i, j int) bool { return ps[i].From < ps[j].From })
	out := []PortRange{ps[0]}
	for _, p := range ps[1:] {
		last := &out[len(out)-1]
		if p.From <= last.To+1 {
			last.To = max(last.To, p.To)
		} else {
			out = append(out, p)
		}
	}
	return out
}

func subtractPorts(a, b []PortRange) []PortRange {
	out := mergePorts(a)
	for _, d := range mergePorts(b) {
		var next []PortRange
		for _, p := range out {
			if d.To < p.From || d.From > p.To {
				next = append(next, p)
				continue
			}
			if p.From < d.From {
				next = append(next, PortRange{From: p.From, To: d.From - 1})
			}
			if p.To > d.To {
				next = append(next, PortRange{From: d.To + 1, To: p.To})
			}
		}
		out = next
	}
	return out
}

// machineError reports an unknown or ambiguous machine name and returns its code.
func machineError(c *collector, path string, x *index, name string) string {
	_, code := x.machine(name)
	switch code {
	case "unknown_machine":
		c.add(path, code, "machine %q is not in this workspace", name)
	case "ambiguous_machine":
		c.add(path, code, "two machines are called %q; write machine:<owner email>/%s", name, name)
	}
	return code
}
