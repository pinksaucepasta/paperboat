package billing

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/transactionalmail"
)

// BandwidthExhaustedEvent is the Activity action shown to the workspace's owner and admins.
const BandwidthExhaustedEvent = "billing.bandwidth_exhausted"

// ConfigureNotificationMail supplies the deployment's mail policy. Without it (no mail
// provider key) the Activity event is still recorded and no mail is queued.
func (s *WorkspaceService) ConfigureNotificationMail(policy config.TransactionalMail, enabled bool) {
	s.mail, s.mailEnabled = policy, enabled
}

// noticeBandwidthExhaustedTx tells a workspace's owner (personal) or owner and admins (team)
// that relay and edge traffic stopped, once per allowance period: the audit event and each
// recipient's mail are idempotent on the period, so every later debit or retry is a no-op.
// It runs in the debit's transaction, so a notice exists if and only if the debit committed.
func (s *WorkspaceService) noticeBandwidthExhaustedTx(ctx context.Context, tx *db.Tx, workspaceID string, window usageWindow) error {
	kind, id, found := strings.Cut(workspaceID, ":")
	if !found || (kind != "personal" && kind != "team") || id == "" {
		return nil
	}
	period := window.budget.UTC().Format(time.RFC3339)
	resource := "user"
	if kind == "team" {
		resource = "team"
	}
	if err := audit.NewWriter(nil).WriteTx(ctx, tx, audit.Event{
		ActorType: audit.ActorSystem, EventType: BandwidthExhaustedEvent, ResourceType: resource, ResourceID: id,
		IdempotencyKey: "bandwidth-exhausted:" + workspaceID + ":" + period,
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
			From: s.mail.From, To: []string{r.address}, Subject: "Paperboat: bandwidth allowance used up",
			HTML: fmt.Sprintf("<p>Relay and edge traffic for %s has stopped because its bandwidth allowance is used up. Direct machine-to-machine traffic keeps working.</p><p>It resumes when the allowance resets on %s (UTC) or as soon as you add bandwidth.</p><p><a href=\"%s\">Add bandwidth or upgrade</a></p>",
				html.EscapeString(r.name), window.end.UTC().Format("2 Jan 2006"), html.EscapeString(billing)),
		}
		if _, err := transactionalmail.EnqueueOnceTx(ctx, tx, "bandwidth-exhausted:"+workspaceID+":"+period+":"+r.account, message); err != nil {
			return err
		}
	}
	return nil
}
