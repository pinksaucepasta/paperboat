package teams

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat/shared/policy"
)

// The simple view (per-machine "Manage access", `pb team machine-grant`, the private-access
// switch) edits the policy file through these functions; the file stays the one owner.

// setPolicyTx applies edit to the current file (with current names) and saves the result as
// an explicit user change. Lock must be held; callers check rights.
func setPolicyTx(ctx context.Context, tx *db.Tx, w Workspace, cfg config.WorkspacePolicy, actor, message string, edit func([]byte, policy.Inventory) ([]byte, error)) error {
	cur, err := policyTx(ctx, tx, w)
	if err != nil {
		return err
	}
	inv, err := policyInventoryTx(ctx, tx, w)
	if err != nil {
		return err
	}
	next, err := edit(external(cur.source, inv), inv)
	if err != nil {
		return err
	}
	_, _, err = savePolicyTx(ctx, tx, w, cfg, policySave{actor: actor, source: next, message: message, kind: "saved", confirmed: true})
	return err
}

// ApplyMachineAccessTx sets what everyone in the team, or one member, may use on one shared
// machine (r.Active false or no capabilities removes it). The caller has checked that actor may
// manage the machine's access and that the machine is bound to the team.
func ApplyMachineAccessTx(ctx context.Context, tx *db.Tx, cfg config.WorkspacePolicy, team, actor string, r MachineGrantRequest) error {
	return setPolicyTx(ctx, tx, Workspace{TeamID: team}, cfg, actor, "Machine access changed", func(ext []byte, inv policy.Inventory) ([]byte, error) {
		bound := false
		for _, m := range inv.Machines {
			bound = bound || m.ID == r.MachineID
		}
		if !bound {
			return nil, ErrForbidden
		}
		subject := "role:member"
		if r.Audience == "selected_member" {
			subject = ""
			for _, m := range inv.Members {
				if m.AccountID == r.AccountID {
					subject = "user:" + strings.ToLower(m.Email)
				}
			}
			if subject == "" {
				return nil, ErrForbidden
			}
		}
		var caps []string
		if r.Active {
			caps = r.Capabilities
		}
		return policy.SetSimpleRule(ext, subject, "machine:"+inv.MachineName(r.MachineID), caps)
	})
}

// envActions is the simple view of a team ENV permission ("" removes the member's rule).
var envActions = map[string][]string{"read": {policy.ActionENVRead}, "write": {policy.ActionENVRead, policy.ActionENVWrite}, "": nil}

// ApplyENVAccessTx sets one member's team ENV permission (read, write, or "" for none) in the
// team policy. The caller has checked rights and that the team ENV exists; losing read takes
// the member's key (envAccessChangedTx).
func ApplyENVAccessTx(ctx context.Context, tx *db.Tx, cfg config.WorkspacePolicy, team, actor, account, permission string) error {
	actions, ok := envActions[permission]
	if !ok {
		return ErrInvalid
	}
	return setPolicyTx(ctx, tx, Workspace{TeamID: team}, cfg, actor, "Team ENV access changed", func(ext []byte, inv policy.Inventory) ([]byte, error) {
		for _, m := range inv.Members {
			if m.AccountID == account {
				return policy.SetSimpleRule(ext, "user:"+strings.ToLower(m.Email), "env:team", actions)
			}
		}
		if permission == "" {
			return ext, nil // a member who left has no rules left (refreshPolicyTx)
		}
		return nil, ErrForbidden
	})
}

// machineGrantsTx is the simple view of a team policy: its per-machine rules as grants.
func machineGrants(p teamPolicy) []MachineGrant {
	out := []MachineGrant{}
	if !p.parsed {
		return out
	}
	machines, accounts := map[string]string{}, map[string]string{}
	for id, name := range p.inv.MachineNames() {
		machines["machine:"+name] = id
	}
	for _, m := range p.inv.Members {
		accounts["user:"+strings.ToLower(m.Email)] = m.AccountID
	}
	for _, r := range policy.SimpleRules(p.document) {
		if r.Target == "env:team" {
			continue
		}
		g := MachineGrant{MachineID: machines[r.Target], Audience: "all_members", Capabilities: r.Allow, Generation: p.version, Active: true}
		if r.Subject != "role:member" {
			g.Audience, g.AccountID = "selected_member", accounts[strings.ToLower(r.Subject)]
		}
		if g.MachineID != "" && (g.Audience == "all_members" || g.AccountID != "") {
			out = append(out, g)
		}
	}
	return out
}

// SetAllowPrivateAccessTx sets the team's "Allow private access" switch in its policy file.
func SetAllowPrivateAccessTx(ctx context.Context, tx *db.Tx, cfg config.WorkspacePolicy, team, actor string, on bool) error {
	message := map[bool]string{true: "Private access turned on", false: "Private access turned off"}[on]
	return setPolicyTx(ctx, tx, Workspace{TeamID: team}, cfg, actor, message, func(ext []byte, _ policy.Inventory) ([]byte, error) {
		return policy.SetAllowPrivateAccess(ext, on)
	})
}

// teamPolicy is a team's stored policy, parsed once against its inventory for the team view.
type teamPolicy struct {
	version  uint64
	inv      policy.Inventory
	document policy.Document
	parsed   bool // false: no policy, or a stored file that no longer parses
}

func teamPolicyTx(ctx context.Context, tx *db.Tx, team string) (teamPolicy, error) {
	var p teamPolicy
	var source string
	err := tx.QueryRow(ctx, `SELECT source,version FROM workspace_policies WHERE team_id=$1`, team).Scan(&source, &p.version)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if p.inv, err = policyInventoryTx(ctx, tx, Workspace{TeamID: team}); err != nil {
		return p, err
	}
	var errs policy.Errors
	p.document, errs = policy.Parse(external(source, p.inv))
	p.parsed = len(errs) == 0
	return p, nil
}

// policyGrants is the simple view of the team ENV and of preview/tunnel inspect/replay: each
// member's own rule as a grant (generation: the policy version).
func policyGrants(p teamPolicy, team string) []Grant {
	out := []Grant{}
	if !p.parsed {
		return out
	}
	inv, d, version := p.inv, p.document, p.version
	accounts := map[string]string{}
	for _, m := range inv.Members {
		accounts["user:"+strings.ToLower(m.Email)] = m.AccountID
	}
	resources := map[string]policy.Resource{}
	for _, r := range inv.Resources {
		resources[resourceLabel(r)] = r
	}
	for _, r := range policy.SimpleRules(d) {
		id := accounts[strings.ToLower(r.Subject)]
		if id == "" {
			continue
		}
		if r.Target == "env:team" {
			permission := "read"
			if slices.Contains(r.Allow, policy.ActionENVWrite) {
				permission = "write"
			}
			out = append(out, Grant{AccountID: id, ResourceKind: "env", ResourceID: team, Permission: permission, Generation: version, Active: true})
		} else if res, ok := resources[r.Target]; ok {
			for _, a := range r.Allow {
				out = append(out, Grant{AccountID: id, ResourceKind: res.Kind, ResourceID: res.ID, Permission: a, Generation: version, Active: true})
			}
		}
	}
	return out
}

// resourceLabel is a preview's or tunnel's name in a policy file (a preview has none: its ID).
func resourceLabel(r policy.Resource) string {
	if r.Name != "" {
		return r.Kind + ":" + r.Name
	}
	return r.Kind + ":" + r.ID
}

// ApplyResourceAccessTx adds (r.Active) or removes r.Permission (inspect or replay) for one member
// on one preview or tunnel bound to the team, in the member's rule for it. The caller has checked
// rights and the binding.
func ApplyResourceAccessTx(ctx context.Context, tx *db.Tx, cfg config.WorkspacePolicy, team, actor string, r GrantRequest) error {
	return setPolicyTx(ctx, tx, Workspace{TeamID: team}, cfg, actor, "Inspector access changed", func(ext []byte, inv policy.Inventory) ([]byte, error) {
		subject, target := "", ""
		for _, m := range inv.Members {
			if m.AccountID == r.AccountID {
				subject = "user:" + strings.ToLower(m.Email)
			}
		}
		for _, res := range inv.Resources {
			if res.Kind == r.ResourceKind && res.ID == r.ResourceID {
				target = resourceLabel(res)
			}
		}
		if subject == "" || target == "" {
			return nil, ErrForbidden
		}
		d, errs := policy.Parse(ext)
		if len(errs) > 0 {
			return nil, &PolicyInvalidError{Errors: errs}
		}
		var actions []string
		for _, s := range policy.SimpleRules(d) {
			if s.Subject == subject && s.Target == target {
				actions = slices.DeleteFunc(slices.Clone(s.Allow), func(a string) bool { return a == r.Permission })
			}
		}
		if r.Active {
			actions = append(actions, r.Permission)
		}
		return policy.SetSimpleRule(ext, subject, target, actions)
	})
}
