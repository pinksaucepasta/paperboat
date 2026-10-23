package teams

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"strings"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat/shared/policy"
)

// ExplainPolicy answers "may src do action on dst, and why", "who may do action on dst" or
// "what may src do", on the saved policy or on a draft. Members may ask only about themselves.
func (s *Service) ExplainPolicy(ctx context.Context, account, ws string, r PolicyExplainRequest) (PolicyExplanation, error) {
	var out PolicyExplanation
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		sc, err := scopeTx(ctx, tx, account, ws)
		if err != nil {
			return err
		}
		inv, err := policyInventoryTx(ctx, tx, sc.w)
		if err != nil {
			return err
		}
		source := ""
		if r.Policy != nil {
			source = *r.Policy
		} else {
			p, err := policyTx(ctx, tx, sc.w)
			if err != nil {
				return err
			}
			source = string(external(p.source, inv))
		}
		token := ""
		if r.APIToken != "" {
			if token, err = apiTokenScopeTx(ctx, tx, sc, r.Src, r.APIToken, &inv); err != nil {
				return err
			}
		}
		_, c, errs := policy.Validate([]byte(source), inv, s.policy.Limits())
		if compileFailed(errs) {
			return &PolicyInvalidError{Errors: errs}
		}
		// Explaining anyone's access is an audit right (owners, admins, auditors); members explain themselves.
		admin := Allowed(sc.actor.Role, PermissionAuditRead)
		var src string
		if r.Src != "" {
			id, ok := c.User(r.Src)
			if !ok {
				return &PolicyInvalidError{Errors: policy.Errors{{Path: "/src", Code: "unknown_user", Message: r.Src + " is not a member of this workspace"}}}
			}
			src = id
		}
		if !admin && src != account {
			return ErrPolicyExplainForbidden
		}
		var target policy.Target
		if r.Dst != "" {
			t, ok := c.Target(r.Dst)
			if !ok {
				return &PolicyInvalidError{Errors: policy.Errors{{Path: "/dst", Code: "unknown_target", Message: r.Dst + " is not in this workspace"}}}
			}
			target = t
		}
		switch {
		case src != "" && r.Dst != "" && r.Action != "":
			d := PolicyDecision{Decision: c.Decide(src, target, r.Action, r.Port)}
			if token != "" {
				d.Decision = c.DecideToken(token, src, target, r.Action, r.Port)
			}
			if g, err := guardrailTx(ctx, tx, sc); err != nil {
				return err
			} else if g != "" && d.Reason != "owner" {
				d.Allowed, d.Guardrail = false, g
			}
			if d.Allowed && sc.w.TeamID != "" {
				// Require SSO (F19-04) is per member: the same guardrail team_machine_bound_for applies.
				var ssoOK bool
				if err := tx.QueryRow(ctx, `SELECT team_member_sso_ok($1,$2)`, sc.w.TeamID, src).Scan(&ssoOK); err != nil {
					return err
				}
				if !ssoOK {
					d.Allowed, d.Guardrail = false, "team_sso_required"
				}
			}
			if d.Allowed && target.Kind == "env" {
				var delivered bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM environment_vault_teams t JOIN environment_vault_team_members e USING(team_id) WHERE t.team_id=$1 AND e.account_id=$2 AND e.grant_epoch=t.key_epoch)`, sc.w.TeamID, src).Scan(&delivered); err != nil {
					return err
				}
				if !delivered {
					d.KeyDelivered = &delivered
				}
			}
			out.Decision = &d
		case r.Dst != "" && r.Action != "":
			out.WhoCan = limitRows(c.WhoCan(target, r.Action), s.policy.ExplainMaxRows)
		case src != "":
			out.WhatCan = limitRows(c.WhatCan(src), s.policy.ExplainMaxRows)
		default:
			return &PolicyInvalidError{Errors: policy.Errors{{Code: "required", Message: "give src, dst and action; or dst and action; or src"}}}
		}
		return nil
	})
	return out, err
}

func limitRows(a []policy.Access, n int) []policy.Access {
	if len(a) > n {
		return a[:n]
	}
	if a == nil {
		return []policy.Access{}
	}
	return a
}

// guardrailTx names a workspace-wide guardrail that denies everything the policy allows.
func guardrailTx(ctx context.Context, tx *db.Tx, sc policyScope) (string, error) {
	if sc.w.TeamID == "" {
		return "", nil
	}
	if sc.team.Suspended {
		return "team_suspended", nil
	}
	var paid bool
	if err := tx.QueryRow(ctx, `SELECT team_paid_entitled($1)`, sc.w.TeamID).Scan(&paid); err != nil {
		return "", err
	}
	if !paid {
		return "team_not_entitled", nil
	}
	return "", nil
}

func (s *Service) PolicyVersions(ctx context.Context, account, ws string) ([]PolicyVersion, error) {
	out := []PolicyVersion{}
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
		rows, err := tx.Query(ctx, `SELECT version,digest,created_at,coalesce(author,''),coalesce(api_token,''),message,gained,lost,kind FROM workspace_policy_versions WHERE policy_id=$1 ORDER BY version DESC`, p.id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v PolicyVersion
			var author string
			if err = rows.Scan(&v.Version, &v.Digest, &v.CreatedAt, &author, &v.APIToken, &v.Message, &v.Gained, &v.Lost, &v.Kind); err != nil {
				return err
			}
			v.Author = userLabel(inv, author)
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, err
}

// compileFailed reports errors other than failing in-file tests: the policy cannot be evaluated.
func compileFailed(errs policy.Errors) bool {
	for _, e := range errs {
		if e.Code != "test_failed" && !strings.HasPrefix(e.Path, "/tests/") {
			return true
		}
	}
	return false
}

// apiTokenScopeTx finds src's live API token by name in this workspace and, when it has a
// scope, adds it to inv so that the explanation applies the scope.
func apiTokenScopeTx(ctx context.Context, tx *db.Tx, sc policyScope, src, name string, inv *policy.Inventory) (string, error) {
	var account string
	for _, m := range inv.Members {
		if "user:"+strings.ToLower(m.Email) == strings.ToLower(src) {
			account = m.AccountID
		}
	}
	var id string
	var scope []byte
	err := tx.QueryRow(ctx, `SELECT id,scope FROM api_tokens WHERE account_id=$1 AND team_id IS NOT DISTINCT FROM nullif($2,'') AND name=$3 AND revoked_at IS NULL AND expires_at>now()`, account, sc.w.TeamID, name).Scan(&id, &scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", &PolicyInvalidError{Errors: policy.Errors{{Path: "/api_token", Code: "unknown_api_token", Message: src + " has no live API token named " + name + " in this workspace"}}}
	}
	if err != nil || scope == nil {
		return id, err
	}
	var entries []policy.ScopeEntry
	if err = json.Unmarshal(scope, &entries); err != nil {
		return "", err
	}
	inv.Tokens = append(inv.Tokens, policy.Token{ID: id, Creator: account, Scope: entries})
	return id, nil
}
