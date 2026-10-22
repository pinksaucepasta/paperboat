package teams

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// Directory sync and SSO (PLAN F19-03..06) change membership through these functions only, inside
// the caller's transaction, which holds the team lock. Every change is audited with the directory
// (or the signing-in account, for JIT) as the actor, and advances the team generation so active
// sessions of a removed member end at once.

var (
	// ErrDirectoryManaged: the member's role and membership are owned by the team's directory.
	ErrDirectoryManaged = errors.New("this member is managed by your company directory; change them there")
	// ErrSSORequired: the team requires a recent SSO sign-in and the account has none.
	ErrSSORequired = errors.New("this team requires signing in through your company's SSO")
	// ErrDomainNotVerified: with Require SSO on, invitations go only to verified domains.
	ErrDomainNotVerified = errors.New("this team requires SSO: invite addresses in its verified domains")
)

func directoryAudit(ctx context.Context, tx *db.Tx, actor, team, action string, metadata map[string]any) error {
	actorType := audit.ActorDirectory
	if actor != "" {
		actorType = audit.ActorUser
	}
	metadata, err := activityMetadata(metadata, true)
	if err != nil {
		return err
	}
	return audit.NewWriter(nil).WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: actorType, EventType: "team." + action, ResourceType: "team", ResourceID: team, Metadata: metadata})
}

// ProvisionMemberTx makes account an active member with role, managed by directoryUser when it is
// set (directory sync) or unmanaged (JIT). actor is empty for the directory. An existing member
// keeps their membership; only a directory-managed role is updated. It reports whether anything changed.
func ProvisionMemberTx(ctx context.Context, tx *db.Tx, team, account, role, directoryUser, actor string) (bool, error) {
	if !assignableRole(role) {
		return false, ErrInvalid
	}
	var owner string
	var current *string
	var managed *string
	err := tx.QueryRow(ctx, `SELECT t.owner_account,m.role,m.directory_user_id FROM teams t LEFT JOIN team_members m ON m.team_id=t.team_id AND m.account_id=$2 AND m.active WHERE t.team_id=$1 AND t.deleted_at IS NULL`, team, account).Scan(&owner, &current, &managed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	switch {
	case current == nil:
		if _, err = tx.Exec(ctx, `INSERT INTO team_members(team_id,account_id,membership_generation,role,active,directory_user_id) VALUES($1,$2,1,$3,true,NULLIF($4,'')) ON CONFLICT(team_id,account_id) DO UPDATE SET membership_generation=team_members.membership_generation+1,role=EXCLUDED.role,active=true,joined_at=now(),directory_user_id=EXCLUDED.directory_user_id,sso_exempt=false`, team, account, role, directoryUser); err != nil {
			return false, err
		}
		action := "jit_provisioned"
		if directoryUser != "" {
			action = "directory_provisioned"
		}
		return true, directoryAudit(ctx, tx, actor, team, action, map[string]any{"target_account": account, "role": role})
	case account == owner || directoryUser == "":
		return false, nil
	case *current == role && managed != nil && *managed == directoryUser:
		return false, nil
	}
	if _, err = tx.Exec(ctx, `UPDATE team_members SET role=$3,directory_user_id=$4 WHERE team_id=$1 AND account_id=$2 AND active`, team, account, role, directoryUser); err != nil {
		return false, err
	}
	if *current == role {
		return true, nil
	}
	return true, directoryAudit(ctx, tx, actor, team, "directory_role", map[string]any{"target_account": account, "role": role})
}

// DeprovisionMemberTx removes a directory-managed member exactly like `pb team remove`. The owner
// is never removed by the directory; that refusal is audited.
func DeprovisionMemberTx(ctx context.Context, tx *db.Tx, team, account string) error {
	var owner string
	if err := tx.QueryRow(ctx, `SELECT owner_account FROM teams WHERE team_id=$1`, team).Scan(&owner); err != nil {
		return err
	}
	if account == owner {
		return directoryAudit(ctx, tx, "", team, "directory_owner_kept", map[string]any{"target_account": account})
	}
	if err := RemoveMemberTx(ctx, tx, team, account); err != nil {
		return err
	}
	return directoryAudit(ctx, tx, "", team, "directory_deprovisioned", map[string]any{"target_account": account})
}

// UnlinkDirectoryTx makes every directory-managed member an ordinary member again (the directory
// was disconnected) and cancels its held invitations.
func UnlinkDirectoryTx(ctx context.Context, tx *db.Tx, team string) error {
	if _, err := tx.Exec(ctx, `UPDATE team_members SET directory_user_id=NULL WHERE team_id=$1 AND directory_user_id IS NOT NULL`, team); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE team_invitations SET cancelled_at=now() WHERE team_id=$1 AND directory_user_id IS NOT NULL AND accepted_at IS NULL AND cancelled_at IS NULL`, team)
	return err
}

// HoldDirectoryInvitationTx keeps a seat for a directory user without a Paperboat account; the
// invitation never expires and turns into membership at their first sign-in with that email.
func HoldDirectoryInvitationTx(ctx context.Context, tx *db.Tx, team, email, role, directoryUser string) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO team_invitations(invitation_id,team_id,recipient_email,created_by,expires_at,role,directory_user_id)
 SELECT $1,t.team_id,$3,t.owner_account,'infinity',$4,$5 FROM teams t WHERE t.team_id=$2 AND t.deleted_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM team_invitations i WHERE i.team_id=$2 AND i.recipient_email=$3 AND i.accepted_at IS NULL AND i.cancelled_at IS NULL)`,
		"inv_"+uuid.NewString(), team, email, role, directoryUser)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	return true, directoryAudit(ctx, tx, "", team, "directory_invited", map[string]any{"role": role})
}

// CancelDirectoryInvitationsTx cancels held directory invitations whose user left the directory.
func CancelDirectoryInvitationsTx(ctx context.Context, tx *db.Tx, team string, keep []string) error {
	_, err := tx.Exec(ctx, `UPDATE team_invitations SET cancelled_at=now() WHERE team_id=$1 AND directory_user_id IS NOT NULL AND accepted_at IS NULL AND cancelled_at IS NULL AND NOT (directory_user_id = ANY($2))`, team, keep)
	return err
}

// AcceptDirectoryInvitationsTx turns held directory invitations for a verified email into
// membership; called at sign-in.
func AcceptDirectoryInvitationsTx(ctx context.Context, tx *db.Tx, account, email string) error {
	if err := Lock(ctx, tx); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT invitation_id,team_id,role,directory_user_id FROM team_invitations WHERE recipient_email=lower($1) AND directory_user_id IS NOT NULL AND accepted_at IS NULL AND cancelled_at IS NULL FOR UPDATE`, email)
	if err != nil {
		return err
	}
	type held struct{ id, team, role, user string }
	var invitations []held
	for rows.Next() {
		var h held
		if err := rows.Scan(&h.id, &h.team, &h.role, &h.user); err != nil {
			rows.Close()
			return err
		}
		invitations = append(invitations, h)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, h := range invitations {
		if _, err = ProvisionMemberTx(ctx, tx, h.team, account, h.role, h.user, ""); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE team_invitations SET recipient_account=$2,accepted_at=now() WHERE invitation_id=$1`, h.id, account); err != nil {
			return err
		}
		if err = advance(ctx, tx, h.team); err != nil {
			return err
		}
	}
	return nil
}

// AdvanceTx moves the team generation after a batch of membership changes.
func AdvanceTx(ctx context.Context, tx *db.Tx, team string) error { return advance(ctx, tx, team) }

// AssignableRole reports whether role may be given to a member (every role but owner).
func AssignableRole(role string) bool { return assignableRole(role) }

// AuditDirectoryChangeTx records an SSO or directory setting change made by account.
func AuditDirectoryChangeTx(ctx context.Context, tx *db.Tx, account, team, action string, metadata map[string]any) error {
	return directoryAudit(ctx, tx, account, team, action, metadata)
}

// SSOGuardTx is the SSO guardrail for one member (PLAN F19-04): with Require SSO on, the team is
// refused without an SSO sign-in within the session length. The owner and SSO-exempt members pass
// (break-glass); each such access without SSO is recorded once a day.
func SSOGuardTx(ctx context.Context, tx *db.Tx, team, account string) error {
	var required, ok bool
	err := tx.QueryRow(ctx, `SELECT t.sso_required,team_member_sso_ok(t.team_id,$2) FROM teams t WHERE t.team_id=$1`, team, account).Scan(&required, &ok)
	if err != nil || !required {
		return err
	}
	if !ok {
		return ErrSSORequired
	}
	tag, err := tx.Exec(ctx, `UPDATE team_members m SET sso_bypass_recorded_at=now() FROM teams t
 WHERE t.team_id=$1 AND m.team_id=t.team_id AND m.account_id=$2 AND m.active
  AND (m.sso_verified_at IS NULL OR m.sso_verified_at <= now()-make_interval(secs=>t.sso_session_seconds))
  AND (m.sso_bypass_recorded_at IS NULL OR m.sso_bypass_recorded_at < now()-interval '1 day')`, team, account)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	return directoryAudit(ctx, tx, account, team, "sso_bypassed", nil)
}

// RecordSSOSignInTx marks an SSO sign-in through organization for every team of it the account
// belongs to (audited per team) and queues JIT provisioning where allowed.
func RecordSSOSignInTx(ctx context.Context, tx *db.Tx, organization, account, email string) error {
	rows, err := tx.Query(ctx, `SELECT record_team_sso_signin($1,$2,$3)`, organization, account, email)
	if err != nil {
		return err
	}
	var verified []string
	for rows.Next() {
		var team string
		if err := rows.Scan(&team); err != nil {
			rows.Close()
			return err
		}
		verified = append(verified, team)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, team := range verified {
		if err := directoryAudit(ctx, tx, account, team, "sso_signed_in", nil); err != nil {
			return err
		}
	}
	return nil
}
