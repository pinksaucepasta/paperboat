package policy

import (
	"regexp"
	"strings"
)

// Inventory is what a policy is compiled against: the workspace's current members, machines,
// previews/tunnels and API tokens. The server builds it inside the transaction that saves or
// recompiles the policy.
type Inventory struct {
	// Team is false for a personal workspace (one member, the owner).
	Team bool
	// TeamID is the env:team target's ID in a team workspace.
	TeamID    string
	Members   []Member
	Machines  []Machine
	Resources []Resource
	// Tokens are the workspace's API tokens that have a scope; an unscoped token acts as its
	// creator and needs no entry.
	Tokens []Token
}

type Member struct {
	AccountID string
	Email     string
	// Role is owner, admin or member.
	Role string
}

type Machine struct {
	ID    string
	Alias string
	// PersonalOwner is the account that owns a personal machine; empty for a team-owned one.
	PersonalOwner string
}

// Resource is a preview or tunnel, the target of inspect and replay.
type Resource struct {
	Kind      string // preview or tunnel
	ID        string
	Name      string
	MachineID string
	Owner     string
}

// Token is an API token with a scope. A token acts as the member who created it; its scope
// narrows that access to the listed actions on the listed targets and never widens it.
type Token struct {
	ID      string
	Creator string
	Scope   []ScopeEntry
}

// ScopeEntry is one part of an API token's scope, in the policy file's names:
// {"dst": ["tag:ci"], "allow": ["exec"]}. Ports narrow private_access as in a rule.
type ScopeEntry struct {
	Dst   []string    `json:"dst"`
	Allow []string    `json:"allow"`
	Ports []PortRange `json:"ports,omitempty"`
}

// Limits bound a policy. The values come from the server configuration (one place, the same
// for every plan); a zero field means that check is off.
type Limits struct {
	MaxBytes          int
	MaxRules          int
	MaxTags           int
	MaxGroups         int
	MaxEntriesPerList int
	MaxTests          int
}

var nameExpression = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidName reports whether s is a valid tag or group name (the part after the prefix):
// lowercase letters, digits, '.', '_' and '-', up to 63 characters.
func ValidName(s string) bool { return nameExpression.MatchString(s) }

// Target is what an access question is about.
type Target struct {
	Kind string `json:"kind"` // machine, preview, tunnel or env
	ID   string `json:"id"`
}

// index is the inventory keyed by the names a policy file uses.
type index struct {
	inv         Inventory
	byEmail     map[string]Member
	byAccount   map[string]Member
	byName      map[string][]Machine // alias and owner-email/alias
	byMachineID map[string]Machine
	resources   map[string]Resource // "preview:<name>" / "tunnel:<name>"
	resourceIDs map[string]Resource // "preview:<id>"
}

func newIndex(inv Inventory) *index {
	x := &index{inv: inv, byEmail: map[string]Member{}, byAccount: map[string]Member{}, byName: map[string][]Machine{},
		byMachineID: map[string]Machine{}, resources: map[string]Resource{}, resourceIDs: map[string]Resource{}}
	for _, m := range inv.Members {
		x.byEmail[strings.ToLower(m.Email)] = m
		x.byAccount[m.AccountID] = m
	}
	for _, m := range inv.Machines {
		x.byMachineID[m.ID] = m
	}
	for _, m := range inv.Machines {
		x.byName[m.Alias] = append(x.byName[m.Alias], m)
		if q := x.qualified(m); q != "" {
			x.byName[q] = append(x.byName[q], m)
		}
	}
	for _, r := range inv.Resources {
		name := r.Name
		if name == "" {
			name = r.ID
		}
		x.resources[r.Kind+":"+name] = r
		x.resourceIDs[r.Kind+":"+r.ID] = r
	}
	return x
}

func (x *index) userLabel(account string) string {
	if m, ok := x.byAccount[account]; ok {
		return "user:" + strings.ToLower(m.Email)
	}
	return "account:" + account
}

func (x *index) targetLabel(t Target) string {
	switch t.Kind {
	case "machine":
		if m, ok := x.byMachineID[t.ID]; ok {
			return "machine:" + x.machineName(m)
		}
	case "preview", "tunnel":
		if r, ok := x.resourceIDs[t.Kind+":"+t.ID]; ok && r.Name != "" {
			return t.Kind + ":" + r.Name
		}
	case "env":
		return "env:team"
	}
	return t.Kind + ":" + t.ID
}

// user resolves "user:<email>" to an account ID.
func (x *index) user(s string) (string, bool) {
	email, ok := strings.CutPrefix(s, "user:")
	if !ok {
		return "", false
	}
	m, ok := x.byEmail[strings.ToLower(email)]
	return m.AccountID, ok
}

// target resolves a single dst ("machine:db1", "preview:<name>", "env:team").
func (x *index) target(s string) (Target, bool) {
	kind, name, _ := strings.Cut(s, ":")
	switch kind {
	case "machine":
		m, code := x.machine(name)
		return Target{Kind: "machine", ID: m.ID}, code == ""
	case "preview", "tunnel":
		r, ok := x.resources[s]
		return Target{Kind: kind, ID: r.ID}, ok
	case "env":
		return Target{Kind: "env", ID: x.inv.TeamID}, name == "team" && x.inv.Team
	}
	return Target{}, false
}

// qualified is "<owner email>/<alias>" for a personal machine whose owner is a member: the
// policy name of a shared machine whose alias another machine of the team also uses (aliases
// are unique per owner, not per team).
func (x *index) qualified(m Machine) string {
	if o, ok := x.byAccount[m.PersonalOwner]; ok && m.PersonalOwner != "" {
		return strings.ToLower(o.Email) + "/" + m.Alias
	}
	return ""
}

// machineName is the alias, except for a personal machine whose alias another machine of the
// workspace also has: that one is "<owner email>/<alias>". A team-owned machine (team aliases
// are unique) always keeps its plain alias.
func (x *index) machineName(m Machine) string {
	if len(x.byName[m.Alias]) > 1 {
		if q := x.qualified(m); q != "" {
			return q
		}
	}
	return m.Alias
}

// machine resolves a machine name (after "machine:"); code is "" or the error code.
func (x *index) machine(name string) (Machine, string) {
	if a, b, ok := strings.Cut(name, "/"); ok {
		name = strings.ToLower(a) + "/" + b
	}
	ms := x.byName[name]
	if len(ms) > 1 {
		var team []Machine
		for _, m := range ms {
			if m.PersonalOwner == "" {
				team = append(team, m)
			}
		}
		if len(team) == 1 {
			ms = team
		}
	}
	switch {
	case len(ms) == 0:
		return Machine{}, "unknown_machine"
	case len(ms) > 1:
		return Machine{}, "ambiguous_machine"
	}
	return ms[0], ""
}

// MachineName is the policy-file name of a machine ("db1", or "ana@example.test/laptop" when
// two machines of the workspace are called laptop).
func (inv Inventory) MachineName(id string) string {
	x := newIndex(inv)
	return x.machineName(x.byMachineID[id])
}
