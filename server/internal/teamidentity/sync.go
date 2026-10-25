package teamidentity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
)

// Sync is one worker tick: read new WorkOS events and mark their teams dirty, reconcile a bounded
// number of dirty teams against WorkOS, and process JIT sign-ins. Events only say which team
// changed; the reconcile reads the organization's whole current state, so missed, repeated or
// reordered events cannot leave a team wrong. A daily full reconcile catches anything else.
func (s *Service) Sync(ctx context.Context) error {
	if s.workos == nil {
		return nil
	}
	// One server process at a time; the lock is released at the end of the tick.
	acquired, release, err := s.lockTick(ctx)
	if err != nil || !acquired {
		return err
	}
	defer release()
	errs := []error{s.readEvents(ctx)}
	if _, err = s.db.SQL().ExecContext(ctx, `UPDATE paperboat.team_identity_state s SET dirty=true FROM paperboat.teams t WHERE t.team_id=s.team_id AND t.workos_organization_id IS NOT NULL AND (s.synced_at IS NULL OR s.synced_at < now()-interval '1 day')`); err != nil {
		errs = append(errs, err)
	}
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT s.team_id FROM paperboat.team_identity_state s JOIN paperboat.teams t USING(team_id) WHERE s.dirty AND t.workos_organization_id IS NOT NULL AND t.deleted_at IS NULL ORDER BY s.synced_at NULLS FIRST LIMIT 5`)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	var dirty []string
	for rows.Next() {
		var team string
		if err := rows.Scan(&team); err == nil {
			dirty = append(dirty, team)
		}
	}
	rows.Close()
	for _, team := range dirty {
		if err := s.Reconcile(ctx, team); err != nil {
			errs = append(errs, fmt.Errorf("reconcile team %s: %w", team, err))
		}
	}
	errs = append(errs, s.processJIT(ctx), s.deleteOrganizations(ctx))
	return errors.Join(errs...)
}

// lockTick takes the session lock that makes one server process run the sync at a time. The lock lives on one
// direct connection until release.
func (s *Service) lockTick(ctx context.Context) (bool, func(), error) {
	conn, err := s.db.Direct().Acquire(ctx)
	if err != nil {
		return false, nil, err
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(160236)`).Scan(&acquired); err != nil || !acquired {
		conn.Release()
		return false, nil, err
	}
	return true, func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock(160236)`); err != nil {
			// A connection that failed to unlock must not go back to the pool with the lock.
			_ = conn.Conn().Close(unlockCtx)
		}
		conn.Release()
	}, nil
}

// deleteOrganizations removes the WorkOS organization of deleted teams, so their SSO connection
// and directory stop and WorkOS stops billing for them.
func (s *Service) deleteOrganizations(ctx context.Context) error {
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT team_id,workos_organization_id FROM paperboat.teams WHERE deleted_at IS NOT NULL AND workos_organization_id IS NOT NULL LIMIT 10`)
	if err != nil {
		return err
	}
	type org struct{ team, id string }
	var orgs []org
	for rows.Next() {
		var o org
		if err := rows.Scan(&o.team, &o.id); err == nil {
			orgs = append(orgs, o)
		}
	}
	rows.Close()
	var errs []error
	for _, o := range orgs {
		if err := s.workos.DeleteOrganization(ctx, o.id); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := s.db.SQL().ExecContext(ctx, `UPDATE paperboat.teams SET workos_organization_id=NULL,sso_required=false WHERE team_id=$1`, o.team); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Service) readEvents(ctx context.Context) error {
	var after string
	_ = s.db.SQL().QueryRowContext(ctx, `SELECT after_event FROM paperboat.workos_event_cursor WHERE singleton`).Scan(&after)
	for range 10 {
		events, next, err := s.workos.Events(ctx, after, 100)
		if err != nil {
			return err
		}
		for _, e := range events {
			if _, err := s.db.SQL().ExecContext(ctx, `UPDATE paperboat.team_identity_state s SET dirty=true FROM paperboat.teams t
 WHERE t.team_id=s.team_id AND (t.workos_organization_id=NULLIF($1,'') OR s.directory->>'id'=NULLIF($2,''))`, e.OrganizationID, e.DirectoryID); err != nil {
				return err
			}
		}
		if next != "" && next != after {
			if _, err := s.db.SQL().ExecContext(ctx, `INSERT INTO paperboat.workos_event_cursor(singleton,after_event) VALUES(true,$1) ON CONFLICT(singleton) DO UPDATE SET after_event=EXCLUDED.after_event,updated_at=now()`, next); err != nil {
				return err
			}
			after = next
		}
		if len(events) < 100 {
			return nil
		}
	}
	return nil
}

// Reconcile reads one team's organization from WorkOS and applies it: domains, connections,
// directory state and, when a directory is connected, its members.
func (s *Service) Reconcile(ctx context.Context, team string) error {
	var org string
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT coalesce(workos_organization_id,'') FROM paperboat.teams WHERE team_id=$1 AND deleted_at IS NULL`, team).Scan(&org); err != nil || org == "" {
		return err
	}
	state, err := s.workos.Organization(ctx, org)
	if err != nil {
		s.syncFailed(ctx, team, err)
		return err
	}
	plan, err := s.planDirectory(ctx, team, state)
	if err == nil && plan.newMembers > 0 && s.seats != nil {
		// Seats are bought before members are added, as the owner (who set up directory sync).
		err = s.seats.EnsureSeats(ctx, plan.owner, team, plan.seatsUsed+plan.newMembers, fmt.Sprintf("dsync-seats-%s-%d", team, plan.seatsUsed+plan.newMembers))
	}
	if err != nil {
		s.syncFailed(ctx, team, err)
		return err
	}
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := teams.Lock(ctx, tx); err != nil {
			return err
		}
		if err := writeState(ctx, tx, team, state); err != nil {
			return err
		}
		return s.applyDirectoryTx(ctx, tx, team, state)
	})
}

func (s *Service) syncFailed(ctx context.Context, team string, cause error) {
	message := "Could not read SSO and directory state from WorkOS; retrying."
	if errors.Is(cause, ErrDirectoryTooLarge) {
		message = ErrDirectoryTooLarge.Error()
	}
	_, _ = s.db.SQL().ExecContext(context.WithoutCancel(ctx), `UPDATE paperboat.team_identity_state SET sync_error=$2 WHERE team_id=$1`, team, message)
}

func writeState(ctx context.Context, tx *db.Tx, team string, state OrganizationState) error {
	domains, _ := json.Marshal(nonNil(state.Domains))
	connections, _ := json.Marshal(nonNil(state.Connections))
	groups, _ := json.Marshal(nonNil(state.Groups))
	var directory []byte
	if state.Directory != nil {
		directory, _ = json.Marshal(state.Directory)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO team_identity_state(team_id,domains,connections,directory,directory_groups,directory_users,synced_at,sync_error,dirty)
 VALUES($1,$2,$3,$4,$5,$6,now(),NULL,false) ON CONFLICT(team_id) DO UPDATE SET domains=EXCLUDED.domains,connections=EXCLUDED.connections,
 directory=EXCLUDED.directory,directory_groups=EXCLUDED.directory_groups,directory_users=EXCLUDED.directory_users,synced_at=now(),sync_error=NULL,dirty=false`,
		team, domains, connections, directory, groups, len(state.Users)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM team_sso_domains WHERE team_id=$1`, team); err != nil {
		return err
	}
	for _, d := range state.Domains {
		if d.State != "verified" {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO team_sso_domains(domain,team_id) VALUES(lower($1),$2) ON CONFLICT(domain) DO NOTHING`, d.Domain, team); err != nil {
			return err
		}
	}
	return nil
}

func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// directoryPlan is the provisioning a reconcile will apply, computed before buying seats.
type directoryPlan struct {
	owner                 string
	seatsUsed, newMembers int
}

// desired maps each directory user that should be a member to its role (PLAN F19-05).
func desired(state OrganizationState, roles map[string]string, mappedOnly bool) map[string]DirectoryUser {
	precedence := map[string]int{"admin": 4, "billing": 3, "auditor": 2, "member": 1}
	out := map[string]DirectoryUser{}
	for _, u := range state.Users {
		if !u.Active || u.Email == "" || !strings.Contains(u.Email, "@") {
			continue
		}
		role, mapped := "member", false
		for _, g := range u.Groups {
			if r, ok := roles[g]; ok {
				mapped = true
				if precedence[r] > precedence[role] {
					role = r
				}
			}
		}
		if mappedOnly && !mapped {
			continue
		}
		u.Groups = []string{role}
		out[u.ID] = u
	}
	return out
}

// planDirectory counts the members a reconcile will add, so their seats can be bought first.
func (s *Service) planDirectory(ctx context.Context, team string, state OrganizationState) (directoryPlan, error) {
	var plan directoryPlan
	if state.Directory == nil {
		return plan, nil
	}
	var mappedOnly bool
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT owner_account,directory_mapped_only FROM paperboat.teams WHERE team_id=$1`, team).Scan(&plan.owner, &mappedOnly); err != nil {
		return plan, err
	}
	roles, err := s.readGroupRoles(ctx, team)
	if err != nil {
		return plan, err
	}
	want := desired(state, roles, mappedOnly)
	emails := make([]string, 0, len(want))
	for _, u := range want {
		emails = append(emails, u.Email)
	}
	err = s.db.SQL().QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM paperboat.team_members WHERE team_id=$1 AND active) +
 (SELECT count(*) FROM paperboat.team_invitations WHERE team_id=$1 AND accepted_at IS NULL AND cancelled_at IS NULL AND expires_at>now()),
 (SELECT count(*) FROM unnest($2::text[]) e WHERE NOT EXISTS(
   SELECT 1 FROM paperboat.team_members m JOIN paperboat.users u ON u.id=m.account_id WHERE m.team_id=$1 AND m.active AND u.primary_email=e)
  AND NOT EXISTS(SELECT 1 FROM paperboat.team_invitations i WHERE i.team_id=$1 AND i.recipient_email=e AND i.accepted_at IS NULL AND i.cancelled_at IS NULL))`,
		team, emails).Scan(&plan.seatsUsed, &plan.newMembers)
	return plan, err
}

func (s *Service) readGroupRoles(ctx context.Context, team string) (map[string]string, error) {
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT group_id,role FROM paperboat.team_directory_group_roles WHERE team_id=$1`, team)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	roles := map[string]string{}
	for rows.Next() {
		var id, role string
		if err := rows.Scan(&id, &role); err != nil {
			return nil, err
		}
		roles[id] = role
	}
	return roles, rows.Err()
}

// applyDirectoryTx makes the team's directory-managed members equal to the directory.
func (s *Service) applyDirectoryTx(ctx context.Context, tx *db.Tx, team string, state OrganizationState) error {
	if state.Directory == nil {
		return teams.UnlinkDirectoryTx(ctx, tx, team)
	}
	var mappedOnly bool
	if err := tx.QueryRow(ctx, `SELECT directory_mapped_only FROM teams WHERE team_id=$1`, team).Scan(&mappedOnly); err != nil {
		return err
	}
	roles := map[string]string{}
	rows, err := tx.Query(ctx, `SELECT group_id,role FROM team_directory_group_roles WHERE team_id=$1`, team)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, role string
		if err := rows.Scan(&id, &role); err != nil {
			rows.Close()
			return err
		}
		roles[id] = role
	}
	rows.Close()
	want := desired(state, roles, mappedOnly)
	changed := false
	ids := make([]string, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		u := want[id]
		var account string
		err := tx.QueryRow(ctx, `SELECT coalesce((SELECT m.account_id FROM team_members m WHERE m.team_id=$1 AND m.active AND m.directory_user_id=$2),
 (SELECT u.id FROM users u WHERE u.primary_email=$3 AND u.status='active' AND u.email_verified ORDER BY u.created_at LIMIT 1),'')`, team, u.ID, u.Email).Scan(&account)
		if err != nil {
			return err
		}
		if account == "" {
			held, err := teams.HoldDirectoryInvitationTx(ctx, tx, team, u.Email, u.Groups[0], u.ID, s.invitationLifetime)
			if err != nil {
				return err
			}
			changed = changed || held
			continue
		}
		did, err := teams.ProvisionMemberTx(ctx, tx, team, account, u.Groups[0], u.ID, "")
		if err != nil {
			return err
		}
		changed = changed || did
	}
	if err = teams.CancelDirectoryInvitationsTx(ctx, tx, team, ids); err != nil {
		return err
	}
	gone, err := tx.Query(ctx, `SELECT account_id FROM team_members WHERE team_id=$1 AND active AND directory_user_id IS NOT NULL AND NOT (directory_user_id = ANY($2))`, team, ids)
	if err != nil {
		return err
	}
	var departed []string
	for gone.Next() {
		var account string
		if err := gone.Scan(&account); err != nil {
			gone.Close()
			return err
		}
		departed = append(departed, account)
	}
	gone.Close()
	for _, account := range departed {
		if err := teams.DeprovisionMemberTx(ctx, tx, team, account); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		return teams.AdvanceTx(ctx, tx, team)
	}
	return nil
}

// processJIT adds people who signed in through a team's SSO (JIT on, verified domain) as members.
func (s *Service) processJIT(ctx context.Context) error {
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT j.team_id,j.account_id,t.owner_account FROM paperboat.team_jit_requests j JOIN paperboat.teams t USING(team_id) ORDER BY j.created_at LIMIT 20`)
	if err != nil {
		return err
	}
	type request struct{ team, account, owner string }
	var requests []request
	for rows.Next() {
		var r request
		if err := rows.Scan(&r.team, &r.account, &r.owner); err == nil {
			requests = append(requests, r)
		}
	}
	rows.Close()
	var errs []error
	for _, r := range requests {
		var used int
		if err := s.db.SQL().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM paperboat.team_members WHERE team_id=$1 AND active)+(SELECT count(*) FROM paperboat.team_invitations WHERE team_id=$1 AND accepted_at IS NULL AND cancelled_at IS NULL AND expires_at>now())`, r.team).Scan(&used); err != nil {
			errs = append(errs, err)
			continue
		}
		if s.seats != nil {
			if err := s.seats.EnsureSeats(ctx, r.owner, r.team, used+1, "jit-seat-"+r.team+"-"+r.account); err != nil {
				errs = append(errs, fmt.Errorf("JIT seat for team %s: %w", r.team, err))
				continue
			}
		}
		errs = append(errs, s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			if err := teams.Lock(ctx, tx); err != nil {
				return err
			}
			if _, err := teams.ProvisionMemberTx(ctx, tx, r.team, r.account, "member", "", r.account); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE team_members SET sso_verified_at=now() WHERE team_id=$1 AND account_id=$2`, r.team, r.account); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM team_jit_requests WHERE team_id=$1 AND account_id=$2`, r.team, r.account); err != nil {
				return err
			}
			return teams.AdvanceTx(ctx, tx, r.team)
		}))
	}
	return errors.Join(errs...)
}

// Interval is how often the worker runs.
func (s *Service) Interval() time.Duration { return s.cfg.WorkOSEventPoll }
