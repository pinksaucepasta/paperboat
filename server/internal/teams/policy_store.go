package teams

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat/shared/policy"
)

// The workspace policy (PLAN F18) is part of team permissions: the store lives here so that a
// save is serialized with membership and machine changes (Lock) and fences decisions through
// the team generation exactly as a grant change did.

var (
	ErrPolicyConflict    = errors.New("the policy changed since it was read; reload it and try again")
	ErrPolicyForbidden   = errors.New("you cannot change this part of the policy")
	ErrPolicyUnavailable = errors.New("this policy version does not exist")
)

// PolicyInvalidError carries the validation errors of a refused policy file.
type PolicyInvalidError struct{ Errors policy.Errors }

func (e *PolicyInvalidError) Error() string { return "the policy file is invalid: " + e.Errors.Error() }

// PolicyUnconfirmedError is returned by a save that changes access without the matching
// confirmation; the caller shows Changes and repeats the request with Confirm.
type PolicyUnconfirmedError struct {
	Changes []policy.Change
	Confirm string
}

func (e *PolicyUnconfirmedError) Error() string {
	return fmt.Sprintf("this save changes %d access entries; confirm them", len(e.Changes))
}

// Workspace names one policy: a team, or a personal workspace by its owner.
type Workspace struct {
	TeamID string
	Owner  string
}

func (w Workspace) column() (string, string) {
	if w.TeamID != "" {
		return "team_id", w.TeamID
	}
	return "owner_account", w.Owner
}

type storedPolicy struct {
	id                 int64
	version            uint64
	source             string
	digest             string
	allowPrivateAccess bool
	updatedAt          time.Time
	updatedBy          string
	apiToken           string
	message            string
}

func digest(source []byte) string {
	sum := sha256.Sum256(source)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// emptyPolicy is version 1 of a workspace that has none yet.
const emptyPolicy = "{\n  \"rules\": []\n}\n"

// teamDefaultPolicy is a new team's version 1: owners and admins use the team ENV, as before
// the policy owned it (the owner keeps it regardless, by the owner guardrail).
const teamDefaultPolicy = "{\n  \"rules\": [\n    {\"src\": [\"role:admin\"], \"dst\": [\"env:team\"], \"allow\": [\"env_read\", \"env_write\"]}\n  ]\n}\n"

// policyTx returns the workspace's current policy, creating version 1 when it has none (a
// personal workspace; teams get theirs when created). Lock must be held.
func policyTx(ctx context.Context, tx *db.Tx, w Workspace) (storedPolicy, error) {
	col, key := w.column()
	var p storedPolicy
	err := tx.QueryRow(ctx, `SELECT id,version,source,digest,allow_private_access,updated_at,coalesce(updated_by,''),coalesce(updated_with_api_token,''),message FROM workspace_policies WHERE `+col+`=$1 FOR UPDATE`, key).
		Scan(&p.id, &p.version, &p.source, &p.digest, &p.allowPrivateAccess, &p.updatedAt, &p.updatedBy, &p.apiToken, &p.message)
	if !errors.Is(err, pgx.ErrNoRows) {
		return p, err
	}
	source := emptyPolicy
	if w.TeamID != "" {
		source = teamDefaultPolicy
	}
	p = storedPolicy{version: 1, source: source, digest: digest([]byte(source))}
	err = tx.QueryRow(ctx, `INSERT INTO workspace_policies(`+col+`,version,source,digest) VALUES($1,1,$2,$3) RETURNING id,updated_at`, key, p.source, p.digest).Scan(&p.id, &p.updatedAt)
	if err != nil {
		return p, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO workspace_policy_versions(policy_id,version,kind,source,digest) VALUES($1,1,'saved',$2,$3)`, p.id, p.source, p.digest); err != nil {
		return p, err
	}
	inv, err := policyInventoryTx(ctx, tx, w)
	if err != nil {
		return p, err
	}
	return p, storeRowsTx(ctx, tx, p.id, compileStored(p.source, inv).Store())
}

// InitTeamPolicyTx gives a team just created (owner already a member) its version 1.
func InitTeamPolicyTx(ctx context.Context, tx *db.Tx, team string) error {
	_, err := policyTx(ctx, tx, Workspace{TeamID: team})
	return err
}

// policyInventoryTx is what a policy of w is compiled against.
func policyInventoryTx(ctx context.Context, tx *db.Tx, w Workspace) (policy.Inventory, error) {
	inv := policy.Inventory{Team: w.TeamID != "", TeamID: w.TeamID}
	var rows pgx.Rows
	var err error
	if w.TeamID != "" {
		rows, err = tx.Query(ctx, `SELECT m.account_id,u.primary_email,CASE WHEN t.owner_account=m.account_id THEN 'owner' ELSE m.role END FROM team_members m JOIN teams t USING(team_id) JOIN users u ON u.id=m.account_id WHERE m.team_id=$1 AND m.active ORDER BY m.account_id`, w.TeamID)
	} else {
		rows, err = tx.Query(ctx, `SELECT id,primary_email,'owner' FROM users WHERE id=$1`, w.Owner)
	}
	if err != nil {
		return inv, err
	}
	for rows.Next() {
		var m policy.Member
		if err = rows.Scan(&m.AccountID, &m.Email, &m.Role); err != nil {
			rows.Close()
			return inv, err
		}
		inv.Members = append(inv.Members, m)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return inv, err
	}
	if w.TeamID != "" {
		rows, err = tx.Query(ctx, `SELECT m.id,m.alias,CASE WHEN m.owner_team_id IS NULL THEN m.user_id ELSE '' END FROM team_resource_bindings b JOIN user_machines m ON m.id=b.resource_id WHERE b.team_id=$1 AND b.resource_kind='machine' AND b.active AND m.deleted_at IS NULL ORDER BY m.id`, w.TeamID)
	} else {
		rows, err = tx.Query(ctx, `SELECT id,alias,user_id FROM user_machines WHERE user_id=$1 AND owner_team_id IS NULL AND deleted_at IS NULL AND revoked_at IS NULL ORDER BY id`, w.Owner)
	}
	if err != nil {
		return inv, err
	}
	defer rows.Close()
	for rows.Next() {
		var m policy.Machine
		if err = rows.Scan(&m.ID, &m.Alias, &m.PersonalOwner); err != nil {
			return inv, err
		}
		inv.Machines = append(inv.Machines, m)
	}
	return inv, rows.Err()
}

// storeRowsTx replaces the enforcement rows of a policy.
func storeRowsTx(ctx context.Context, tx *db.Tx, id int64, s policy.Stored) error {
	for _, table := range []string{"workspace_policy_rules", "workspace_policy_groups", "workspace_policy_tags"} {
		if _, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE policy_id=$1`, id); err != nil {
			return err
		}
	}
	for _, r := range s.Rows {
		for _, p := range r.Ports {
			if _, err := tx.Exec(ctx, `INSERT INTO workspace_policy_rules(policy_id,rule,deny,subject_kind,subject_id,target_kind,target_id,action,ports) VALUES($1,$2,$3,$4,$5,$6,$7,$8,int4range($9,$10,'[]'))`,
				id, r.Rule, r.Deny, r.SubjectKind, r.SubjectID, r.TargetKind, r.TargetID, r.Action, p.From, p.To); err != nil {
				return err
			}
		}
	}
	for name, accounts := range s.Groups {
		for _, a := range accounts {
			if _, err := tx.Exec(ctx, `INSERT INTO workspace_policy_groups(policy_id,group_name,account_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, id, name, a); err != nil {
				return err
			}
		}
	}
	for name, machines := range s.Tags {
		for _, m := range machines {
			if _, err := tx.Exec(ctx, `INSERT INTO workspace_policy_tags(policy_id,tag,machine_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, id, name, m); err != nil {
				return err
			}
		}
	}
	return nil
}

// external renders a stored file (machine:#<id>, user:#<account>) with current names.
func external(source string, inv policy.Inventory) []byte {
	out, err := policy.Externalize([]byte(source), inv)
	if err != nil {
		return []byte(source)
	}
	return out
}

// compileStored compiles a stored policy for comparison. A stored file always compiles after
// Externalize dropped departed references; if it does not (a bug), it compares as empty.
func compileStored(source string, inv policy.Inventory) policy.Compiled {
	d, errs := policy.Parse(external(source, inv))
	if len(errs) == 0 {
		if c, errs := policy.Compile(d, inv, policy.Limits{}); len(errs) == 0 {
			return c
		}
	}
	empty, _ := policy.Compile(policy.Document{}, inv, policy.Limits{})
	return empty
}

// confirmation is the digest of a change list: a save with changes must quote it.
func confirmation(changes []policy.Change) string {
	b, _ := json.Marshal(changes)
	return digest(b)
}

// policySave is one write of a policy file.
type policySave struct {
	actor    string // account; empty for a system rewrite
	apiToken string
	source   []byte
	expected uint64 // 0 skips the version check (system rewrites)
	message  string
	confirm  string // ignored when confirmed is true
	kind     string // saved, restored or rewritten
	// confirmed skips the access-change confirmation: system rewrites (renames, removals) and
	// the simple view's own edits, which the user made explicitly.
	confirmed bool
}

// compileForSave validates a file being saved. A system rewrite (a machine unshared, a member
// gone) only drops references, so it is compiled without the file's tests, which may name
// what is gone.
func compileForSave(in policySave, inv policy.Inventory, limits policy.Limits) (policy.Compiled, policy.Errors) {
	if in.kind != "rewritten" {
		_, c, errs := policy.Validate(in.source, inv, limits)
		return c, errs
	}
	d, errs := policy.Parse(in.source)
	if len(errs) > 0 {
		return policy.Compiled{}, errs
	}
	return policy.Compile(d, inv, policy.Limits{})
}

// refreshPolicyTx stores the policy again after machines or members left the workspace,
// without their references (a machine shared again or a member who rejoins starts with no
// access, as with grants before). Lock must be held.
func refreshPolicyTx(ctx context.Context, tx *db.Tx, w Workspace, actor string) error {
	cur, err := policyTx(ctx, tx, w)
	if err != nil {
		return err
	}
	inv, err := policyInventoryTx(ctx, tx, w)
	if err != nil {
		return err
	}
	ext := external(cur.source, inv)
	if in, err := policy.Internalize(ext, inv); err != nil || string(in) == cur.source {
		return err
	}
	_, _, err = savePolicyTx(ctx, tx, w, config.WorkspacePolicy{}, policySave{actor: actor, source: ext, message: "Removed machines or members that left the workspace", kind: "rewritten", confirmed: true})
	return err
}

// savePolicyTx validates and stores a new version. Lock must be held; callers check rights.
// cfg bounds the file; its zero value (system rewrites) checks no limits and prunes nothing.
func savePolicyTx(ctx context.Context, tx *db.Tx, w Workspace, cfg config.WorkspacePolicy, in policySave) (storedPolicy, []policy.Change, error) {
	cur, err := policyTx(ctx, tx, w)
	if err != nil {
		return cur, nil, err
	}
	if in.expected != 0 && in.expected != cur.version {
		return cur, nil, ErrPolicyConflict
	}
	inv, err := policyInventoryTx(ctx, tx, w)
	if err != nil {
		return cur, nil, err
	}
	next, errs := compileForSave(in, inv, cfg.Limits())
	if len(errs) > 0 {
		return cur, nil, &PolicyInvalidError{Errors: errs}
	}
	changes := policy.Changes(compileStored(cur.source, inv), next)
	if len(changes) > 0 && !in.confirmed && in.confirm != confirmation(changes) {
		return cur, changes, &PolicyUnconfirmedError{Changes: changes, Confirm: confirmation(changes)}
	}
	internal, err := policy.Internalize(in.source, inv)
	if err != nil {
		return cur, nil, err
	}
	in.source = internal
	if digest(in.source) == cur.digest {
		return cur, nil, nil
	}
	gained, lost := 0, 0
	for _, c := range changes {
		if c.Change == "gained" {
			gained++
		} else {
			lost++
		}
	}
	next0 := cur
	next0.version, next0.source, next0.digest, next0.message = cur.version+1, string(in.source), digest(in.source), in.message
	next0.updatedBy, next0.apiToken = in.actor, in.apiToken
	next0.allowPrivateAccess = next.AllowPrivateAccess && w.TeamID != ""
	// Rules first: advancing the version runs the trigger that revokes sessions the new rules
	// no longer allow, and tells the team's machines to refresh their access.
	if err = storeRowsTx(ctx, tx, cur.id, next.Store()); err != nil {
		return cur, nil, err
	}
	err = tx.QueryRow(ctx, `UPDATE workspace_policies SET version=$2,source=$3,digest=$4,allow_private_access=$8,updated_at=now(),updated_by=nullif($5,''),updated_with_api_token=nullif($6,''),message=$7 WHERE id=$1 RETURNING updated_at`,
		cur.id, next0.version, next0.source, next0.digest, in.actor, in.apiToken, in.message, next0.allowPrivateAccess).Scan(&next0.updatedAt)
	if err != nil {
		return cur, nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO workspace_policy_versions(policy_id,version,kind,source,digest,author,api_token,message,gained,lost) VALUES($1,$2,$3,$4,$5,nullif($6,''),nullif($7,''),$8,$9,$10)`,
		cur.id, next0.version, in.kind, next0.source, next0.digest, in.actor, in.apiToken, in.message, gained, lost); err != nil {
		return cur, nil, err
	}
	if cfg.VersionsKept > 0 {
		if _, err = tx.Exec(ctx, `DELETE FROM workspace_policy_versions WHERE policy_id=$1 AND version<=$2`, cur.id, int64(next0.version)-int64(cfg.VersionsKept)); err != nil {
			return cur, nil, err
		}
	}
	event := "policy." + map[string]string{"saved": "updated", "restored": "restored", "rewritten": "rewritten"}[in.kind]
	metadata := map[string]any{"version": next0.version, "gained": gained, "lost": lost, "api_token": in.apiToken}
	if w.TeamID == "" {
		err = audit.NewWriter(nil).WriteTx(ctx, tx, audit.Event{ActorUserID: in.actor, ActorType: audit.ActorUser, EventType: "personal." + event, ResourceType: "workspace_policy", ResourceID: w.Owner, IdempotencyKey: "policy:" + w.Owner + ":" + fmt.Sprint(next0.version), Metadata: metadata})
		return next0, changes, err
	}
	if err = advance(ctx, tx, w.TeamID); err != nil {
		return cur, nil, err
	}
	if err = envAccessChangedTx(ctx, tx, w.TeamID, changes); err != nil {
		return cur, nil, err
	}
	err = AuditTx(ctx, tx, in.actor, w.TeamID, event, "policy:"+w.TeamID+":"+fmt.Sprint(next0.version), metadata)
	return next0, changes, err
}

// envAccessChangedTx applies a policy change to the team ENV: a member the policy no longer
// lets read it loses their key exactly as `pb env team revoke` takes it (rotation required when
// they held one); any ENV change invalidates the team's ENV layers.
func envAccessChangedTx(ctx context.Context, tx *db.Tx, team string, changes []policy.Change) error {
	changed := false
	for _, c := range changes {
		changed = changed || c.Target == "env:team"
	}
	if !changed {
		return nil
	}
	var held bool
	err := tx.QueryRow(ctx, `WITH lost AS (
	  UPDATE environment_vault_team_members e SET grant_epoch=0
	  WHERE e.team_id=$1 AND e.grant_epoch>0 AND team_env_permission(e.account_id,e.team_id)=''
	  RETURNING e.account_id
	), dropped AS (DELETE FROM environment_vault_team_grants g USING lost WHERE g.team_id=$1 AND g.account_id=lost.account_id)
	SELECT EXISTS(SELECT 1 FROM lost)`, team).Scan(&held)
	if err != nil {
		return err
	}
	if held {
		return FenceENVTx(ctx, tx, team)
	}
	return InvalidateENVTx(ctx, tx, team)
}
