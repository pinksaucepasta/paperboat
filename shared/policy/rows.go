package policy

import (
	"sort"
	"strings"
)

// Row is one rule entry as the server stores it for enforcement: subjects and targets stay
// symbolic (a role, every member, a group, a tag, every machine) so that members joining and
// machines being shared need no recompilation; the server's SQL check resolves them per
// request with the same meaning as Decide. Account, machine and resource names are resolved to
// IDs. Groups and Tags list their members by ID.
type Row struct {
	Rule        int         `json:"rule"`
	Deny        bool        `json:"deny"`
	SubjectKind string      `json:"subject_kind"` // all, role, account or group
	SubjectID   string      `json:"subject_id"`   // "", owner/admin/member, account ID, group name
	TargetKind  string      `json:"target_kind"`  // all_machines, machine, tag, preview, tunnel or env
	TargetID    string      `json:"target_id"`    // "", machine ID, tag name, resource ID, team ID
	Action      string      `json:"action"`
	Ports       []PortRange `json:"ports"` // private_access only; other actions every port
}

// Stored is what the server writes for one policy version.
type Stored struct {
	Rows               []Row
	Groups             map[string][]string // group name → account IDs
	Tags               map[string][]string // tag name → machine IDs
	AllowPrivateAccess bool
}

// Store returns the enforcement form of a compiled policy.
func (c Compiled) Store() Stored {
	return Stored{Rows: c.rows, Groups: c.groups, Tags: c.tags, AllowPrivateAccess: c.AllowPrivateAccess}
}

func symbolicRows(x *index, idx int, r Rule) []Row {
	list, deny := r.Actions()
	var actions []string
	for _, a := range list {
		if a == "*" {
			actions = append(actions, MachineActions...)
		} else {
			actions = append(actions, a)
		}
	}
	ports := []PortRange{DefaultPrivatePorts}
	if r.Ports != nil {
		ports = mergePorts(r.Ports)
	} else if deny {
		ports = allPorts
	}
	var out []Row
	for _, s := range r.Src {
		sk, sid := symbolicSubject(x, s)
		for _, d := range r.Dst {
			tk, tid := symbolicTarget(x, d)
			for _, a := range actions {
				row := Row{Rule: idx, Deny: deny, SubjectKind: sk, SubjectID: sid, TargetKind: tk, TargetID: tid, Action: a, Ports: allPorts}
				if a == ActionPrivateAccess {
					row.Ports = ports
				}
				out = append(out, row)
			}
		}
	}
	return out
}

func symbolicSubject(x *index, s string) (string, string) {
	if s == "*" {
		return "all", ""
	}
	kind, name, _ := strings.Cut(s, ":")
	switch kind {
	case "user":
		a, _ := x.user(s)
		return "account", a
	case "group":
		return "group", s
	}
	return "role", name
}

func symbolicTarget(x *index, d string) (string, string) {
	switch {
	case d == "machine:*":
		return "all_machines", ""
	case strings.HasPrefix(d, "tag:"):
		return "tag", d
	}
	t, _ := x.target(d)
	return t.Kind, t.ID
}

func sortRows(rows []Row) {
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Rule < rows[j].Rule })
}

// ScopeRows is the enforcement form of an API token scope, like Store for rules: machines by
// ID, tags and every machine by name. A scope narrows; an entry without ports covers every port
// of private_access (the creator's own ports still apply).
func (c Compiled) ScopeRows(scope []ScopeEntry) ([]Row, Errors) {
	if errs := c.ValidateScope(scope); len(errs) > 0 {
		return nil, errs
	}
	var out []Row
	for i, e := range scope {
		r := Rule{Src: []string{"*"}, Dst: e.Dst, Allow: e.Allow, Ports: e.Ports}
		if r.Ports == nil {
			r.Ports = allPorts
		}
		for _, row := range symbolicRows(c.x, i, r) {
			row.SubjectKind, row.SubjectID = "token", ""
			out = append(out, row)
		}
	}
	return out, nil
}
