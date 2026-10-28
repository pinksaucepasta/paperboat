package accountdeletion

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
)

// Preview is what deleting the account will remove, for the console to show before confirmation.
type Preview struct {
	AccountEmail             string        `json:"account_email"`
	Machines                 int           `json:"machines"`
	EnvVaults                int           `json:"env_vaults"`
	Previews                 int           `json:"previews"`
	Tunnels                  int           `json:"tunnels"`
	ConfigSync               bool          `json:"config_sync"`
	InboxItems               int           `json:"inbox_items"`
	APITokens                int           `json:"api_tokens"`
	Subscription             *Subscription `json:"subscription"`
	TeamsDeleted             []TeamRef     `json:"teams_deleted"`
	TeamsBlocking            []TeamBlock   `json:"teams_blocking"`
	ReauthenticatedAt        *time.Time    `json:"reauthenticated_at"`
	ReauthenticationRequired bool          `json:"reauthentication_required"`
}

// Subscription is the account's active personal subscription; its invoices and payments are kept.
type Subscription struct {
	Plan      string     `json:"plan"`
	PeriodEnd *time.Time `json:"period_end"`
}

// Preview counts the account's data and reports the teams that are deleted or block deletion.
func (s *Service) Preview(ctx context.Context, user auth.User, signedInAt time.Time) (Preview, error) {
	out := Preview{AccountEmail: user.PrimaryEmail, TeamsDeleted: []TeamRef{}, TeamsBlocking: []TeamBlock{}}
	if !signedInAt.IsZero() {
		at := signedInAt
		out.ReauthenticatedAt = &at
	}
	out.ReauthenticationRequired = s.checkReauthentication(signedInAt) != nil
	counts := []struct {
		dst   *int
		query string
	}{
		{&out.Machines, `SELECT count(*) FROM live_user_machines WHERE user_id=$1 AND owner_team_id IS NULL`},
		{&out.EnvVaults, `SELECT count(*) FROM environment_vault_scopes WHERE workspace_id='personal' AND owner_kind='personal' AND owner_id=$1`},
		{&out.Previews, `SELECT count(*) FROM preview_leases WHERE account_id=$1 AND owner_team_id IS NULL AND terminal_state='active'`},
		{&out.Tunnels, `SELECT count(*) FROM tunnels WHERE account_id=$1 AND owner_team_id IS NULL AND deleted_at IS NULL`},
		{&out.InboxItems, `SELECT count(*) FROM team_inbox_requests WHERE (sender_account=$1 OR recipient_account=$1) AND status IN ('pending','approved','consumed')`},
		{&out.APITokens, `SELECT count(*) FROM api_tokens WHERE account_id=$1 AND revoked_at IS NULL AND expires_at>now()`},
	}
	for _, c := range counts {
		if err := s.db.SQL().QueryRowContext(ctx, c.query, user.ID).Scan(c.dst); err != nil {
			return Preview{}, fmt.Errorf("count account data: %w", err)
		}
	}
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM control_config_repositories WHERE owner_user_id=$1 AND state<>'disconnected')
		OR EXISTS(SELECT 1 FROM control_config_assignments a JOIN user_machines m ON m.id=a.machine_id WHERE m.user_id=$1 AND m.owner_team_id IS NULL AND a.revoked_at IS NULL)`, user.ID).Scan(&out.ConfigSync); err != nil {
		return Preview{}, fmt.Errorf("check config sync: %w", err)
	}
	sub, err := s.activeSubscription(ctx, user.ID)
	if err != nil {
		return Preview{}, err
	}
	out.Subscription = sub
	owned, err := s.ownedTeams(ctx, user.ID)
	if err != nil {
		return Preview{}, err
	}
	for _, t := range owned {
		if t.Members > 1 {
			out.TeamsBlocking = append(out.TeamsBlocking, t)
		} else {
			out.TeamsDeleted = append(out.TeamsDeleted, TeamRef{Slug: t.Slug, Name: t.Name})
		}
	}
	return out, nil
}

// activeSubscription is the personal workspace's subscription that has not ended.
func (s *Service) activeSubscription(ctx context.Context, userID string) (*Subscription, error) {
	var plan sql.NullString
	var periodEnd sql.NullTime
	err := s.db.SQL().QueryRowContext(ctx, `SELECT product_code, period_end FROM billing_workspaces
		WHERE user_id=$1 AND team_id IS NULL AND provider_subscription_id IS NOT NULL AND state NOT IN ('canceled','incomplete_expired')
		ORDER BY id LIMIT 1`, userID).Scan(&plan, &periodEnd)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read subscription: %w", err)
	}
	out := &Subscription{Plan: plan.String}
	if periodEnd.Valid {
		at := periodEnd.Time
		out.PeriodEnd = &at
	}
	return out, nil
}

// ownedTeams lists the teams the account owns that are not deleted, with their active member counts.
func (s *Service) ownedTeams(ctx context.Context, userID string) ([]TeamBlock, error) {
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT t.team_id, coalesce(t.display_name,''), (SELECT count(*) FROM team_members m WHERE m.team_id=t.team_id AND m.active)
		FROM teams t WHERE t.owner_account=$1 AND t.deleted_at IS NULL ORDER BY t.team_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list owned teams: %w", err)
	}
	defer rows.Close()
	var out []TeamBlock
	for rows.Next() {
		var t TeamBlock
		var name string
		if err := rows.Scan(&t.Slug, &name, &t.Members); err != nil {
			return nil, fmt.Errorf("read owned team: %w", err)
		}
		t.Name = name
		if t.Name == "" {
			t.Name = t.Slug
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
