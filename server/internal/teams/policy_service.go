package teams

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat/shared/policy"
)

// ErrPolicyExplainForbidden: a member asked about someone else's access.
var ErrPolicyExplainForbidden = errors.New("members can explain only their own access")

type PolicyNames struct {
	Users    []string `json:"users"`
	Machines []string `json:"machines"`
	Previews []string `json:"previews"`
	Tunnels  []string `json:"tunnels"`
	Tags     []string `json:"tags"`
	Groups   []string `json:"groups"`
}

type PolicyView struct {
	Workspace           string      `json:"workspace"`
	Version             uint64      `json:"version"`
	Policy              string      `json:"policy"`
	Digest              string      `json:"digest"`
	UpdatedAt           time.Time   `json:"updated_at"`
	UpdatedBy           string      `json:"updated_by,omitempty"`
	UpdatedWithAPIToken string      `json:"updated_with_api_token,omitempty"`
	Message             string      `json:"message"`
	CanEdit             string      `json:"can_edit"`
	Names               PolicyNames `json:"names"`
}

type PolicyTests struct {
	Passed int `json:"passed"`
	Failed int `json:"failed"`
}

type PolicyValidation struct {
	Valid         bool            `json:"valid"`
	Errors        policy.Errors   `json:"errors"`
	Tests         PolicyTests     `json:"tests"`
	AccessChanges []policy.Change `json:"access_changes"`
	Confirm       string          `json:"confirm,omitempty"`
}

type PolicySaveRequest struct {
	Policy          string `json:"policy"`
	ExpectedVersion uint64 `json:"expected_version"`
	Message         string `json:"message,omitempty"`
	Confirm         string `json:"confirm,omitempty"`
}

type PolicyRestoreRequest struct {
	Version         uint64 `json:"version"`
	ExpectedVersion uint64 `json:"expected_version"`
	Message         string `json:"message,omitempty"`
	Confirm         string `json:"confirm,omitempty"`
}

type PolicyExplainRequest struct {
	Policy *string `json:"policy,omitempty"`
	Src    string  `json:"src,omitempty"`
	Dst    string  `json:"dst,omitempty"`
	Action string  `json:"action,omitempty"`
	Port   int     `json:"port,omitempty"`
	// APIToken names one of src's API tokens: the answer then includes the token's scope.
	APIToken string `json:"api_token,omitempty"`
}

type PolicyDecision struct {
	policy.Decision
	Guardrail string `json:"guardrail,omitempty"`
}

type PolicyExplanation struct {
	Decision *PolicyDecision `json:"decision,omitempty"`
	WhoCan   []policy.Access `json:"who_can,omitempty"`
	WhatCan  []policy.Access `json:"what_can,omitempty"`
}

type PolicyVersion struct {
	Version   uint64    `json:"version"`
	Digest    string    `json:"digest"`
	CreatedAt time.Time `json:"created_at"`
	Author    string    `json:"author,omitempty"`
	APIToken  string    `json:"api_token,omitempty"`
	Message   string    `json:"message"`
	Gained    int       `json:"gained"`
	Lost      int       `json:"lost"`
	Kind      string    `json:"kind"`
}

// policyScope is one request's view of a workspace: who is asking and what they may do.
type policyScope struct {
	w       Workspace
	id      string // "personal" or the team slug
	team    Team   // zero for personal
	actor   Member
	canEdit string // all, own_machines or none
	active  bool   // false while the team is suspended: reads only
}

// scopeTx resolves ws for account under Lock.
func scopeTx(ctx context.Context, tx *db.Tx, account, ws string) (policyScope, error) {
	if err := Lock(ctx, tx); err != nil {
		return policyScope{}, err
	}
	if ws == "personal" {
		return policyScope{w: Workspace{Owner: account}, id: ws, actor: Member{AccountID: account, Role: "owner"}, canEdit: "all", active: true}, nil
	}
	t, err := ReadTx(ctx, tx, ws)
	if err != nil {
		return policyScope{}, err
	}
	m, ok := currentMember(t, account)
	if !ok || t.Deleted {
		return policyScope{}, ErrForbidden
	}
	if t.OwnerAccount == account {
		m.Role = "owner"
	}
	sc := policyScope{w: Workspace{TeamID: ws}, id: ws, team: t, actor: m, canEdit: "none", active: !t.Suspended}
	switch {
	case Allowed(m.Role, PermissionSettingsManage):
		sc.canEdit = "all"
	case slices.ContainsFunc(t.Machines, func(b MachineBinding) bool { return b.Active && b.OwnerAccount == account }):
		sc.canEdit = "own_machines"
	}
	return sc, nil
}

func (s *Service) Policy(ctx context.Context, account, ws string, version uint64) (PolicyView, error) {
	var out PolicyView
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		sc, err := scopeTx(ctx, tx, account, ws)
		if err != nil {
			return err
		}
		p, err := policyTx(ctx, tx, sc.w)
		if err != nil {
			return err
		}
		if version != 0 && version != p.version {
			var kind string
			err = tx.QueryRow(ctx, `SELECT source,digest,created_at,coalesce(author,''),coalesce(api_token,''),message,kind FROM workspace_policy_versions WHERE policy_id=$1 AND version=$2`, p.id, version).
				Scan(&p.source, &p.digest, &p.updatedAt, &p.updatedBy, &p.apiToken, &p.message, &kind)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrPolicyUnavailable
			}
			if err != nil {
				return err
			}
			p.version = version
		}
		inv, err := policyInventoryTx(ctx, tx, sc.w)
		if err != nil {
			return err
		}
		out = viewOf(sc, p, inv)
		return nil
	})
	return out, err
}

func viewOf(sc policyScope, p storedPolicy, inv policy.Inventory) PolicyView {
	names := PolicyNames{Users: []string{}, Machines: []string{}, Previews: []string{}, Tunnels: []string{}, Tags: []string{}, Groups: []string{}}
	for _, m := range inv.Members {
		names.Users = append(names.Users, strings.ToLower(m.Email))
	}
	for _, m := range inv.Machines {
		names.Machines = append(names.Machines, inv.MachineName(m.ID))
	}
	if d, errs := policy.Parse(external(p.source, inv)); len(errs) == 0 {
		for t := range d.Tags {
			names.Tags = append(names.Tags, t)
		}
		for g := range d.Groups {
			names.Groups = append(names.Groups, g)
		}
	}
	slices.Sort(names.Tags)
	slices.Sort(names.Groups)
	return PolicyView{Workspace: sc.id, Version: p.version, Policy: string(external(p.source, inv)), Digest: p.digest, UpdatedAt: p.updatedAt,
		UpdatedBy: userLabel(inv, p.updatedBy), UpdatedWithAPIToken: p.apiToken, Message: p.message, CanEdit: sc.canEdit, Names: names}
}

func userLabel(inv policy.Inventory, account string) string {
	for _, m := range inv.Members {
		if m.AccountID == account {
			return "user:" + strings.ToLower(m.Email)
		}
	}
	return ""
}

// ValidatePolicy is a dry run: errors, test results and the access changes against the saved
// version.
func (s *Service) ValidatePolicy(ctx context.Context, account, ws string, source []byte) (PolicyValidation, error) {
	var out PolicyValidation
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		sc, err := scopeTx(ctx, tx, account, ws)
		if err != nil {
			return err
		}
		p, err := policyTx(ctx, tx, sc.w)
		if err != nil {
			return err
		}
		inv, err := policyInventoryTx(ctx, tx, sc.w)
		if err != nil {
			return err
		}
		d, next, errs := policy.Validate(source, inv, s.policy.Limits())
		out = PolicyValidation{Valid: len(errs) == 0, Errors: errs, AccessChanges: []policy.Change{}}
		if out.Errors == nil {
			out.Errors = policy.Errors{}
		}
		failed := map[string]bool{}
		for _, e := range errs {
			if parts := strings.SplitN(e.Path, "/", 4); len(parts) >= 3 && parts[1] == "tests" {
				failed[parts[2]] = true
			}
		}
		if !compileFailed(errs) {
			out.Tests = PolicyTests{Passed: len(d.Tests) - len(failed), Failed: len(failed)}
			out.AccessChanges = policy.Changes(compileStored(p.source, inv), next)
			if out.AccessChanges == nil {
				out.AccessChanges = []policy.Change{}
			}
			if len(out.AccessChanges) > 0 {
				out.Confirm = confirmation(out.AccessChanges)
			}
		}
		return nil
	})
	return out, err
}

func (s *Service) SavePolicy(ctx context.Context, account, ws string, r PolicySaveRequest) (PolicyView, error) {
	return s.writePolicy(ctx, account, ws, func(context.Context, *db.Tx, Workspace, storedPolicy) (policySave, error) {
		return policySave{actor: account, source: []byte(r.Policy), expected: r.ExpectedVersion, message: r.Message, confirm: r.Confirm, kind: "saved"}, nil
	})
}

func (s *Service) RestorePolicy(ctx context.Context, account, ws string, r PolicyRestoreRequest) (PolicyView, error) {
	return s.writePolicy(ctx, account, ws, func(ctx context.Context, tx *db.Tx, w Workspace, cur storedPolicy) (policySave, error) {
		var source string
		err := tx.QueryRow(ctx, `SELECT source FROM workspace_policy_versions WHERE policy_id=$1 AND version=$2`, cur.id, r.Version).Scan(&source)
		if errors.Is(err, pgx.ErrNoRows) {
			return policySave{}, ErrPolicyUnavailable
		}
		message := r.Message
		if message == "" {
			message = "Restored version " + strconv.FormatUint(r.Version, 10)
		}
		if err != nil {
			return policySave{}, err
		}
		inv, err := policyInventoryTx(ctx, tx, w)
		return policySave{actor: account, source: external(source, inv), expected: r.ExpectedVersion, message: message, confirm: r.Confirm, kind: "restored"}, err
	})
}

func (s *Service) writePolicy(ctx context.Context, account, ws string, build func(context.Context, *db.Tx, Workspace, storedPolicy) (policySave, error)) (PolicyView, error) {
	var out PolicyView
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		sc, err := scopeTx(ctx, tx, account, ws)
		if err != nil {
			return err
		}
		if !sc.active || sc.canEdit == "none" {
			return ErrPolicyForbidden
		}
		cur, err := policyTx(ctx, tx, sc.w)
		if err != nil {
			return err
		}
		in, err := build(ctx, tx, sc.w, cur)
		if err != nil {
			return err
		}
		if sc.canEdit == "own_machines" {
			inv, err := policyInventoryTx(ctx, tx, sc.w)
			if err != nil {
				return err
			}
			if !onlyOwnMachines(string(external(cur.source, inv)), string(in.source), ownMachineNames(sc, inv)) {
				return ErrPolicyForbidden
			}
		}
		p, _, err := savePolicyTx(ctx, tx, sc.w, s.policy, in)
		if err != nil {
			return err
		}
		inv, err := policyInventoryTx(ctx, tx, sc.w)
		if err != nil {
			return err
		}
		out = viewOf(sc, p, inv)
		return nil
	})
	return out, err
}

func ownMachineNames(sc policyScope, inv policy.Inventory) map[string]bool {
	own := map[string]bool{}
	for _, m := range inv.Machines {
		if m.PersonalOwner == sc.actor.AccountID {
			own["machine:"+inv.MachineName(m.ID)] = true
		}
	}
	return own
}

// onlyOwnMachines reports whether next differs from cur only in rules whose every dst is one
// of own: the right a member keeps over access to the machines they shared.
func onlyOwnMachines(cur, next string, own map[string]bool) bool {
	a, errsA := policy.Parse([]byte(cur))
	b, errsB := policy.Parse([]byte(next))
	if len(errsA) > 0 || len(errsB) > 0 {
		return false
	}
	ra, rb := a.Rules, b.Rules
	a.Rules, b.Rules = nil, nil
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		return false
	}
	count := map[string]int{}
	rules := map[string]policy.Rule{}
	for _, r := range ra {
		k, _ := json.Marshal(r)
		count[string(k)]++
		rules[string(k)] = r
	}
	for _, r := range rb {
		k, _ := json.Marshal(r)
		count[string(k)]--
		rules[string(k)] = r
	}
	for k, n := range count {
		if n == 0 {
			continue
		}
		for _, d := range rules[k].Dst {
			if !own[d] {
				return false
			}
		}
	}
	return true
}
