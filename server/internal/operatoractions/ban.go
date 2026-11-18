package operatoractions

import (
	"context"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/accountdeletion"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
)

// lockSignInTx takes the lock a sign-in holds while it resolves the account's identity (the canonical e-mail), so an
// account ban or unban waits for a sign-in in flight and then revokes the session it created, never the other way round.
// It must be taken before the account row is locked, in the sign-in's order.
func lockSignInTx(ctx context.Context, tx *db.Tx, account string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(coalesce(canonical_email,paperboat.canonical_email_address(primary_email)),174)) FROM users WHERE id=$1`, account)
	return err
}

// banAccount is what an operator's ban of an account adds to the suspension accountAction already made (status suspended,
// sessions, tokens and access ended): the ban record with the identity that may not sign in or sign up again, the account's
// peer certificates revoked so its machines lose their network access, the teams it owns suspended with it, its paid
// subscriptions scheduled to end (when the operator chose), and the networks the operator chose to block. Its data stays; a
// machine's previews and tunnels stop being served because the edge answers the taken-down page for the owner's hostnames.
func (s *Service) banAccount(ctx context.Context, tx *db.Tx, actor, target, reason string, in Input, out *Result) error {
	banID, err := activeBanTx(ctx, tx, "user", target)
	if err != nil {
		return err
	}
	if banID != "" {
		return ErrConflict // already banned: unban first to change the ban
	}
	banID = "ban_" + uuid.NewString()
	inserted, err := tx.Exec(ctx, `INSERT INTO bans(id,subject_kind,subject_id,reason,banned_by,canonical_email,workos_subject,subscriptions)
 SELECT $1,'user',u.id,$3,$4,coalesce(u.canonical_email,paperboat.canonical_email_address(u.primary_email)),u.workos_subject,$5 FROM users u WHERE u.id=$2 ON CONFLICT DO NOTHING`,
		banID, target, reason, actor, subscriptionsMode(in))
	if err != nil {
		return err
	}
	if inserted.RowsAffected() != 1 {
		return ErrConflict
	}
	if err = s.noticeBannedTx(ctx, tx, target, banID, "your account", reason); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE peer_endpoint_certificates SET revoked_at=now(),revocation_reason='account_revoked' WHERE user_id=$1 AND revoked_at IS NULL`, target); err != nil {
		return err
	}
	owned, err := ownedTeamsTx(ctx, tx, target)
	if err != nil {
		return err
	}
	for _, team := range owned {
		if team.suspended {
			continue
		}
		if _, err = teams.SetOperatorSuspensionTx(ctx, tx, actor, team.id, true, team.generation); err != nil {
			return mapTeamError(err)
		}
		caused, err := tx.Exec(ctx, `INSERT INTO bans(id,subject_kind,subject_id,reason,banned_by,subscriptions,cause_ban_id) VALUES($1,'team',$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`,
			"ban_"+uuid.NewString(), team.id, reason, actor, subscriptionsMode(in), banID)
		if err != nil {
			return err
		}
		out.Teams += caused.RowsAffected()
	}
	ids := make([]string, 0, len(owned))
	for _, team := range owned {
		ids = append(ids, team.id)
	}
	if err = scheduleSubscriptionsTx(ctx, tx, target, ids, in.Subscriptions); err != nil {
		return err
	}
	return blockBanNetworks(ctx, tx, actor, reason, in.BlockIPs)
}

// unbanAccount lifts the ban record (the identity may sign in again) and the team bans its owner's ban caused. Machines
// re-pair with a new sign-in (`pb login`); subscriptions that were cancelled stay cancelled.
func (s *Service) unbanAccount(ctx context.Context, tx *db.Tx, actor, target string, out *Result) error {
	var banID string
	err := tx.QueryRow(ctx, `UPDATE bans SET lifted_at=now(),lifted_by=$2 WHERE subject_kind='user' AND subject_id=$1 AND lifted_at IS NULL RETURNING id`, target, actor).Scan(&banID)
	if operatorNoRowsOnly(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// Every team ban this ban caused is lifted, also one whose team was deleted since; a team still suspended is restored.
	rows, err := tx.Query(ctx, `SELECT b.id,b.subject_id,coalesce(t.generation,0),coalesce(t.suspended AND t.deleted_at IS NULL,false) FROM bans b
 LEFT JOIN teams t ON t.team_id=b.subject_id WHERE b.cause_ban_id=$1 AND b.subject_kind='team' AND b.lifted_at IS NULL ORDER BY b.subject_id FOR UPDATE OF b`, banID)
	if err != nil {
		return err
	}
	type caused struct {
		ban, team  string
		generation int64
		restore    bool
	}
	var list []caused
	for rows.Next() {
		var c caused
		if err = rows.Scan(&c.ban, &c.team, &c.generation, &c.restore); err != nil {
			rows.Close()
			return err
		}
		list = append(list, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, c := range list {
		if c.restore {
			if _, err = teams.SetOperatorSuspensionTx(ctx, tx, actor, c.team, false, c.generation); err != nil {
				return mapTeamError(err)
			}
			out.Teams++
		}
		if _, err = tx.Exec(ctx, `UPDATE bans SET lifted_at=now(),lifted_by=$2 WHERE id=$1`, c.ban, actor); err != nil {
			return err
		}
	}
	return nil
}

// banTeam records an operator's ban of a team (the suspension itself is teams.SetOperatorSuspensionTx) and schedules the
// team's paid subscription to end when the operator chose.
func (s *Service) banTeam(ctx context.Context, tx *db.Tx, actor, target, reason string, in Input) error {
	id, err := activeBanTx(ctx, tx, "team", target)
	if err != nil {
		return err
	}
	if id != "" {
		return ErrConflict
	}
	banID := "ban_" + uuid.NewString()
	inserted, err := tx.Exec(ctx, `INSERT INTO bans(id,subject_kind,subject_id,reason,banned_by,subscriptions) VALUES($1,'team',$2,$3,$4,$5) ON CONFLICT DO NOTHING`,
		banID, target, reason, actor, subscriptionsMode(in))
	if err != nil {
		return err
	}
	if inserted.RowsAffected() != 1 {
		return ErrConflict
	}
	var owner string
	if err = tx.QueryRow(ctx, `SELECT owner_account FROM teams WHERE team_id=$1`, target).Scan(&owner); err != nil {
		return err
	}
	if err = s.noticeBannedTx(ctx, tx, owner, banID, "your team "+target, reason); err != nil {
		return err
	}
	return scheduleSubscriptionsTx(ctx, tx, owner, []string{target}, in.Subscriptions)
}

func (s *Service) unbanTeam(ctx context.Context, tx *db.Tx, actor, target string) error {
	_, err := tx.Exec(ctx, `UPDATE bans SET lifted_at=now(),lifted_by=$2 WHERE subject_kind='team' AND subject_id=$1 AND lifted_at IS NULL`, target, actor)
	return err
}

func subscriptionsMode(in Input) string {
	if in.Subscriptions == "" {
		return "keep"
	}
	return in.Subscriptions
}

func activeBanTx(ctx context.Context, tx *db.Tx, kind, id string) (string, error) {
	var ban string
	err := tx.QueryRow(ctx, `SELECT id FROM bans WHERE subject_kind=$1 AND subject_id=$2 AND lifted_at IS NULL`, kind, id).Scan(&ban)
	if operatorNoRowsOnly(err) {
		return "", nil
	}
	return ban, err
}

type ownedTeam struct {
	id         string
	generation int64
	suspended  bool
}

// ownedTeamsTx locks the live teams the account owns.
func ownedTeamsTx(ctx context.Context, tx *db.Tx, account string) ([]ownedTeam, error) {
	rows, err := tx.Query(ctx, `SELECT team_id,generation,suspended FROM teams WHERE owner_account=$1 AND deleted_at IS NULL ORDER BY team_id FOR UPDATE`, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ownedTeam
	for rows.Next() {
		var t ownedTeam
		if err = rows.Scan(&t.id, &t.generation, &t.suspended); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// scheduleSubscriptionsTx queues the provider work that ends the paid subscriptions of the account's personal workspace and of
// the given teams: now ("immediate") or when the paid period ends ("period_end"). The outbox runs it after commit with retries, so
// a provider outage never undoes the ban. "keep" or empty queues nothing.
func scheduleSubscriptionsTx(ctx context.Context, tx *db.Tx, account string, teamIDs []string, mode string) error {
	kind := ""
	switch mode {
	case "period_end":
		kind = accountdeletion.JobStripeCancelAtPeriodEnd
	case "immediate":
		kind = accountdeletion.JobStripeCancelNow
	default:
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO account_deletion_jobs(id,user_id,kind,target)
 SELECT 'adj_'||gen_random_uuid()::text,$1,$3,s.id FROM (SELECT DISTINCT provider_subscription_id id FROM billing_workspaces
  WHERE (user_id=$1 OR team_id=ANY($2::text[])) AND provider_subscription_id IS NOT NULL AND state NOT IN ('canceled','incomplete_expired')) s
 ON CONFLICT (user_id,kind,target) DO UPDATE SET state='queued',attempts=0,last_error='',next_attempt_at=now(),lease_until=NULL,completed_at=NULL WHERE account_deletion_jobs.state='failed'`, account, teamIDs, kind)
	return err
}
