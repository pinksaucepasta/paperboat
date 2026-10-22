package policy

import (
	"slices"
	"strings"
)

// compileRule checks one rule and adds its grants to allow or deny. Every dst of a rule must
// be of one family (machines, one preview/tunnel, or ENV), and every action must apply to it.
func compileRule(c *collector, allow, deny *builder, x *index, groups map[string][]string, tags map[string][]string, p string, idx int, r Rule) {
	before := len(c.errs)
	list, denies := r.Actions()
	field := map[bool]string{false: "allow", true: "deny"}[denies]
	for _, f := range []struct {
		name string
		list []string
	}{{"src", r.Src}, {"dst", r.Dst}, {field, list}} {
		if len(f.list) == 0 {
			c.add(pointer(p, f.name), "empty", "%q must list at least one entry", f.name)
		}
	}
	accounts := ruleSubjects(c, x, groups, pointer(p, "src"), r.Src)
	family, targets := ruleTargets(c, x, tags, pointer(p, "dst"), r.Dst)
	actions := ruleActions(c, family, pointer(p, field), list)
	ports := rulePorts(c, p, actions, r.Ports, denies)
	if len(c.errs) > before {
		return
	}
	b := map[bool]*builder{false: allow, true: deny}[denies]
	for _, a := range accounts {
		for _, t := range targets {
			for _, action := range actions {
				b.add(a, t, action, ports, idx)
			}
		}
	}
}

// rulePorts checks a rule's ports. Without ports, an allow covers ports 1024 and above (as
// automatic private access does today) and a deny covers every port.
func rulePorts(c *collector, p string, actions []string, ports []PortRange, deny bool) []PortRange {
	if ports == nil {
		if deny {
			return allPorts
		}
		return []PortRange{DefaultPrivatePorts}
	}
	if !slices.Equal(actions, []string{ActionPrivateAccess}) {
		c.add(pointer(p, "ports"), "ports_need_private_access", "ports apply only when the actions are exactly [\"private_access\"]")
	}
	if len(ports) == 0 {
		c.add(pointer(p, "ports"), "empty", "\"ports\" must list at least one port; leave it out for the default")
	}
	return mergePorts(ports)
}

func ruleSubjects(c *collector, x *index, groups map[string][]string, p string, src []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(a string) {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for i, e := range src {
		ep := pointer(p, i)
		kind, name, _ := strings.Cut(e, ":")
		switch {
		case e == "*":
			for _, m := range x.inv.Members {
				add(m.AccountID)
			}
		case kind == "user":
			if a, ok := resolveUser(c, ep, x, e, ""); ok {
				add(a)
			}
		case kind == "group":
			members, ok := groups[e]
			if !ok {
				c.add(ep, "unknown_group", "group %q is not defined in \"groups\"", e)
			}
			for _, a := range members {
				add(a)
			}
		case kind == "role":
			if name != "owner" && name != "admin" && name != "member" {
				c.add(ep, "unknown_role", "roles are role:owner, role:admin (owners and admins) and role:member (everyone)")
				continue
			}
			for _, m := range x.inv.Members {
				if name == "member" || m.Role == "owner" || (name == "admin" && m.Role == "admin") {
					add(m.AccountID)
				}
			}
		default:
			c.add(ep, "invalid_subject", "%q is not a subject; use user:<email>, group:<name>, role:<owner|admin|member> or *", e)
		}
	}
	return out
}

// ruleTargets returns the rule's family ("machine", "resource" or "env") and its targets.
func ruleTargets(c *collector, x *index, tags map[string][]string, p string, dst []string) (string, []Target) {
	family := ""
	seen := map[Target]bool{}
	var out []Target
	add := func(t Target) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for i, e := range dst {
		ep := pointer(p, i)
		kind, name, _ := strings.Cut(e, ":")
		f := ""
		switch kind {
		case "machine":
			f = "machine"
			if name == "*" {
				for _, m := range x.inv.Machines {
					add(Target{Kind: "machine", ID: m.ID})
				}
			} else if m, ok := x.byAlias[name]; ok {
				add(Target{Kind: "machine", ID: m.ID})
			} else {
				c.add(ep, "unknown_machine", "machine %q is not in this workspace", name)
			}
		case "tag":
			f = "machine"
			ids, ok := tags[e]
			if !ok {
				c.add(ep, "unknown_tag", "tag %q is not defined in \"tags\"", e)
			}
			for _, id := range ids {
				add(Target{Kind: "machine", ID: id})
			}
		case "preview", "tunnel":
			f = "resource"
			if r, ok := x.resources[e]; ok {
				add(Target{Kind: kind, ID: r.ID})
			} else {
				c.add(ep, "unknown_"+kind, "%s %q is not in this workspace", kind, name)
			}
		case "env":
			f = "env"
			if !x.inv.Team {
				c.add(ep, "team_only", "env:team exists only in a team workspace")
			} else if name != "team" {
				c.add(ep, "unknown_env_scope", "the ENV target is env:team")
			} else {
				add(Target{Kind: "env", ID: x.inv.TeamID})
			}
		default:
			c.add(ep, "invalid_target", "%q is not a target; use machine:<alias>, machine:*, tag:<name>, preview:<name>, tunnel:<name> or env:team", e)
			continue
		}
		if family == "" {
			family = f
		} else if f != family {
			c.add(ep, "mixed_targets", "one rule's dst must be all machines/tags, all previews/tunnels, or env:team; split it into two rules")
		}
	}
	return family, out
}

func ruleActions(c *collector, family, p string, allow []string) []string {
	valid := map[string][]string{"machine": MachineActions, "resource": ResourceActions, "env": ENVActions}[family]
	var out []string
	for i, a := range allow {
		ep := pointer(p, i)
		switch {
		case a == "*" && family == "machine":
			out = append(out, MachineActions...)
		case slices.Contains(valid, a):
			out = append(out, a)
		case family == "":
			// dst already failed; the action cannot be checked against it.
		case slices.Contains(MachineActions, a) || slices.Contains(ENVActions, a) || a == "*":
			c.add(ep, "action_not_for_target", "%q does not apply to these targets; they allow: %s", a, strings.Join(valid, ", "))
		default:
			c.add(ep, "unknown_action", "unknown action %q; actions are %s, %s", a, strings.Join(MachineActions, ", "), strings.Join(ENVActions, ", "))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
