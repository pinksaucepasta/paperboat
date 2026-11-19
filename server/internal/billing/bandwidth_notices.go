// Package billing implements plans, workspaces and billing on Stripe: Free, Pro and Team subscriptions
// with seat quantities, checkout and portal sessions, trials and grace periods, bandwidth top-ups and
// usage periods, and the webhook handlers that reconcile Stripe's state. Operator products and prices
// are read through the PriceReader. Uncertain provider outcomes are recorded so they can be reconciled
// instead of repeated.
package billing

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/announcements"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/transactionalmail"
)

// BandwidthThrottledEvent is the Activity action shown to the workspace's owner and admins when its fair-use
// bandwidth is used up and its edge and relay traffic slows down.
const BandwidthThrottledEvent = "billing.bandwidth_throttled"

// ConfigureNotificationMail supplies the deployment's mail policy. Without it (no mail
// provider key) the Activity event is still recorded and no mail is queued.
func (s *WorkspaceService) ConfigureNotificationMail(policy config.TransactionalMail, enabled bool) {
	s.mail, s.mailEnabled = policy, enabled
}

// SetThrottleRates tells the notices and the overview how fast a throttled workspace's public traffic runs (the edges get
// the same values, as per-edge shares, from the same server configuration).
func (s *WorkspaceService) SetThrottleRates(fair config.FairUse) { s.throttle = fair }

// throttleMbit is the speed of a throttled workspace in Mbit/s: Free's, or a paid plan's.
func (s *WorkspaceService) throttleMbit(paid bool) float64 {
	if paid {
		return float64(s.throttle.EdgeBytesPerSecond) * 8 / 1e6
	}
	return float64(s.throttle.EdgeFreeBytesPerSecond) * 8 / 1e6
}

// speed words a speed for people: "512 kbit/s", "1 Mbit/s".
func speed(mbit float64) string {
	if mbit < 1 {
		return fmt.Sprintf("%g kbit/s", mbit*1000)
	}
	return fmt.Sprintf("%g Mbit/s", mbit)
}

func gigabytes(bytes int64) string { return fmt.Sprintf("%g GB", float64(bytes)/1e9) }

// workspaceAudience is who the bell and the mail of a workspace's notices go to: the account (personal) or the team's
// members. ok is false for an id that is neither.
func workspaceAudience(workspaceID string) (kind, id string, ok bool) {
	prefix, id, found := strings.Cut(workspaceID, ":")
	if !found || id == "" || (prefix != "personal" && prefix != "team") {
		return "", "", false
	}
	if prefix == "team" {
		return announcements.AudienceTeam, id, true
	}
	return announcements.AudienceUser, id, true
}

// noticeBandwidthWarningTx puts "80% of fair-use bandwidth used" in the workspace's bell, once per allowance period.
func (s *WorkspaceService) noticeBandwidthWarningTx(ctx context.Context, tx *db.Tx, workspaceID string, window usageWindow, allowance int64) error {
	mbit := s.throttleMbit(window.paid)
	audience, id, ok := workspaceAudience(workspaceID)
	// A delayed usage report can debit a period that is over already: nothing to warn about then.
	if !ok || !window.end.After(time.Now()) {
		return nil
	}
	return announcements.SystemNoticeTx(ctx, tx, audience, id, "bandwidth-80:"+workspaceID+":"+window.budget.UTC().Format(time.RFC3339),
		"80% of included traffic used",
		fmt.Sprintf("After %s a month through Paperboat's edges and relays, traffic is reduced to %s. Add bandwidth to keep full speed.", gigabytes(allowance), speed(mbit)),
		"/billing", &window.end)
}

// noticeBandwidthResetTx tells a workspace whose previous allowance period ended throttled that full speed is back. It runs
// when the new period's row is created, so it fires once per period, and only after a throttled one.
func noticeBandwidthResetTx(ctx context.Context, tx *db.Tx, workspaceID string, window usageWindow) error {
	audience, id, ok := workspaceAudience(workspaceID)
	if !ok {
		return nil
	}
	var previous sql.NullTime
	if err := tx.QueryRow(ctx, `SELECT max(coalesce(budget_start,period_start)) FROM billing_workspace_usage_periods
WHERE workspace_id=$1 AND coalesce(budget_start,period_start)<$2`, workspaceID, window.budget).Scan(&previous); err != nil || !previous.Valid {
		return err
	}
	var throttled bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.announcements WHERE created_by=$1 AND idempotency_key=$2)`,
		announcements.SystemCreator, "bandwidth-throttled:"+workspaceID+":"+previous.Time.UTC().Format(time.RFC3339)).Scan(&throttled); err != nil || !throttled {
		return err
	}
	ends := time.Now().AddDate(0, 0, 14)
	return announcements.SystemNoticeTx(ctx, tx, audience, id, "bandwidth-reset:"+workspaceID+":"+window.budget.UTC().Format(time.RFC3339),
		"Full speed is back", "A new billing period started. Traffic through Paperboat's edges and relays runs at full speed again.", "/billing", &ends)
}

// noticeBandwidthThrottledTx tells a workspace's owner (personal) or owner and admins (team) that edge and relay traffic is slowed,
// once per allowance period: the bell notice, the audit event and each recipient's mail are idempotent on the period, so
// every later debit or retry is a no-op. It runs in the debit's transaction, so a notice exists if and only if the debit
// committed.
func (s *WorkspaceService) noticeBandwidthThrottledTx(ctx context.Context, tx *db.Tx, workspaceID string, window usageWindow, allowance int64) error {
	mbit := s.throttleMbit(window.paid)
	kind, id, found := strings.Cut(workspaceID, ":")
	// A delayed usage report can debit a period that is over already: its notices would never expire.
	if !found || (kind != "personal" && kind != "team") || id == "" || !window.end.After(time.Now()) {
		return nil
	}
	period := window.budget.UTC().Format(time.RFC3339)
	audience, _, _ := workspaceAudience(workspaceID)
	if err := announcements.SystemNoticeTx(ctx, tx, audience, id, "bandwidth-throttled:"+workspaceID+":"+period,
		fmt.Sprintf("Traffic reduced to %s", speed(mbit)),
		fmt.Sprintf("This workspace used its %s of included traffic (edges and relays together). Full speed returns on %s, or sooner if you add bandwidth. Nothing is cut off.", gigabytes(allowance), window.end.UTC().Format("2 Jan")),
		"/billing", &window.end); err != nil {
		return err
	}
	resource := "user"
	if kind == "team" {
		resource = "team"
	}
	if err := audit.NewWriter(nil).WriteTx(ctx, tx, audit.Event{
		ActorType: audit.ActorSystem, EventType: BandwidthThrottledEvent, ResourceType: resource, ResourceID: id,
		IdempotencyKey: "bandwidth-throttled:" + workspaceID + ":" + period,
		Metadata:       map[string]any{"workspace": workspaceID, "resets_at": window.end.UTC().Format(time.RFC3339)},
	}); err != nil {
		return err
	}
	if !s.mailEnabled {
		return nil
	}
	query := `SELECT u.id,u.primary_email,'your personal workspace' FROM users u WHERE u.id=$1 AND u.status='active' AND u.email_verified AND u.primary_email<>''`
	if kind == "team" {
		query = `SELECT u.id,u.primary_email,coalesce(t.display_name,t.team_id) FROM teams t JOIN team_members m ON m.team_id=t.team_id AND m.active
JOIN users u ON u.id=m.account_id WHERE t.team_id=$1 AND t.deleted_at IS NULL AND (team_role_allows(CASE WHEN t.owner_account=m.account_id THEN 'owner' ELSE m.role END,'members.manage') OR team_role_allows(CASE WHEN t.owner_account=m.account_id THEN 'owner' ELSE m.role END,'billing.manage'))
AND u.status='active' AND u.email_verified AND u.primary_email<>'' ORDER BY u.id LIMIT 64`
	}
	rows, err := tx.Query(ctx, query, id)
	if err != nil {
		return err
	}
	type recipient struct{ account, address, name string }
	var recipients []recipient
	for rows.Next() {
		var r recipient
		if err := rows.Scan(&r.account, &r.address, &r.name); err != nil {
			rows.Close()
			return err
		}
		recipients = append(recipients, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	billing := strings.TrimRight(s.mail.ConsoleOrigin, "/") + "/billing"
	for _, r := range recipients {
		message := transactionalmail.Message{
			From: s.mail.From, To: []string{r.address}, Subject: "Paperboat: traffic reduced",
			HTML: fmt.Sprintf("<p>Traffic through Paperboat's edges and relays for %s is reduced to %s because its included traffic is used up. It is never cut off and never billed extra. Direct machine-to-machine traffic and your own edges and relays are not slowed.</p><p>Full speed returns on %s (UTC), or as soon as you add bandwidth.</p><p><a href=\"%s\">Add bandwidth or upgrade</a></p>",
				html.EscapeString(r.name), speed(mbit), window.end.UTC().Format("2 Jan 2006"), html.EscapeString(billing)),
		}
		if _, err := transactionalmail.EnqueueOnceTx(ctx, tx, "bandwidth-throttled:"+workspaceID+":"+period+":"+r.account, message); err != nil {
			return err
		}
	}
	return nil
}
