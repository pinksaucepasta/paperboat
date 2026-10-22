package billing

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/transactionalmail"
)

// TrialWillEndEvent is Stripe's notice, three days before a trial ends.
const TrialWillEndEvent = "customer.subscription.trial_will_end"

// ConfigureTeamTrial sets the Team trial length; 0 creates teams without a trial.
func (s *WorkspaceService) ConfigureTeamTrial(days int) {
	s.teamTrialDays = max(days, 0)
}

// teamTrialDaysTx gives an account one Team trial: none once it has created a paid team.
func (s *WorkspaceService) teamTrialDaysTx(ctx context.Context, tx *db.Tx, actor string) (int, error) {
	if s.teamTrialDays == 0 {
		return 0, nil
	}
	var used bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_team_orders WHERE actor_user_id=$1 AND state='paid')`, actor).Scan(&used)
	if err != nil || used {
		return 0, err
	}
	return s.teamTrialDays, nil
}

// noticeTeamTrialEnding mails a trialing team's owner and admins that billing starts at
// the trial's end and that cancelling before then costs nothing. Once per subscription.
func (s *WorkspaceService) noticeTeamTrialEnding(ctx context.Context, subscriptionID string) error {
	if !s.mailEnabled {
		return nil
	}
	return s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		rows, err := tx.Query(ctx, `SELECT u.id,u.primary_email,coalesce(t.display_name,t.team_id),w.period_end,w.quantity
FROM billing_workspaces w JOIN teams t ON t.team_id=w.team_id AND t.deleted_at IS NULL
JOIN team_members m ON m.team_id=t.team_id AND m.active AND (t.owner_account=m.account_id OR m.role='admin')
JOIN users u ON u.id=m.account_id AND u.status='active' AND u.email_verified AND u.primary_email<>''
WHERE w.provider_subscription_id=$1 AND w.state='trialing' AND NOT w.cancel_at_period_end AND w.period_end IS NOT NULL
ORDER BY u.id LIMIT 64`, subscriptionID)
		if err != nil {
			return err
		}
		type recipient struct {
			account, address, team string
			ends                   time.Time
			seats                  int
		}
		var recipients []recipient
		for rows.Next() {
			var r recipient
			if err := rows.Scan(&r.account, &r.address, &r.team, &r.ends, &r.seats); err != nil {
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
				From: s.mail.From, To: []string{r.address}, Subject: "Paperboat: your Team trial ends in 3 days",
				HTML: fmt.Sprintf("<p>The Team trial for %s ends on %s (UTC). Billing for %d seat(s) starts then on the card you added.</p><p>Cancel before then and you will not be charged.</p><p><a href=\"%s\">Manage team billing</a></p>",
					html.EscapeString(r.team), r.ends.UTC().Format("2 Jan 2006"), r.seats, html.EscapeString(billing)),
			}
			if _, err := transactionalmail.EnqueueOnceTx(ctx, tx, "team-trial-ending:"+subscriptionID+":"+r.account, message); err != nil {
				return err
			}
		}
		return nil
	})
}
