package billing

import (
	"context"
	"fmt"
	"html"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/legalconsent"
	"github.com/pinksaucepasta/paperboat/server/internal/transactionalmail"
)

// noticeTeamTrialStartedTx confirms a Team trial to the buyer. Stripe emails receipts, renewal and trial-ending
// reminders itself, but a trial moves no money, so nothing of Stripe's confirms the contract: this mail carries
// the price, the first charge date, how to cancel and the consent the buyer accepted (Directive 2011/83/EU art. 8(7)-(8)).
func (s *WorkspaceService) noticeTeamTrialStartedTx(ctx context.Context, tx *db.Tx, actor, operation string, req teamCreationRequest, sub StripeSubscription) error {
	if !s.mailEnabled {
		return nil
	}
	var address string
	err := tx.QueryRow(ctx, `SELECT primary_email FROM users WHERE id=$1 AND status='active' AND email_verified AND primary_email<>''`, actor).Scan(&address)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var amount int64
	var currency string
	// A confirmation must never stop the team being created: without a price row there is no mail.
	if err = tx.QueryRow(ctx, `SELECT verified_unit_amount,verified_currency FROM billing_products WHERE provider='stripe' AND provider_price_id=$1`, req.PriceID).Scan(&amount, &currency); err == pgx.ErrNoRows {
		return nil
	} else if err != nil {
		return err
	}
	var version string
	if err = tx.QueryRow(ctx, `SELECT terms_version FROM billing_terms_consents WHERE operation_id=$1`, operation).Scan(&version); err != nil && err != pgx.ErrNoRows {
		return err
	}
	// shortcut: amounts print with two decimals, right for the USD catalog; zero-decimal currencies need their own exponent.
	total := amount * int64(sub.Quantity)
	body := fmt.Sprintf("<p>Your Team trial for %s has started with %d seat(s).</p>"+
		"<p>Nothing is charged during the trial. Billing starts on %s (UTC) on the card you added: %d.%02d %s per %s for the %d seat(s), and the team renews automatically until you cancel.</p>"+
		"<p>To cancel, open Billing in the console (%s) before then and you will not be charged. After a payment, cancelling stops the next renewal and the plan keeps working until the period you paid for ends.</p>",
		html.EscapeString(req.Input.TeamID), sub.Quantity, sub.PeriodEnd.UTC().Format("2 Jan 2006"), total/100, total%100, html.EscapeString(strings.ToUpper(currency)),
		html.EscapeString(req.Input.Interval), sub.Quantity, html.EscapeString(strings.TrimRight(s.mail.ConsoleOrigin, "/")+"/billing"))
	if version != "" {
		body += fmt.Sprintf("<p>You accepted this at checkout (consent version %s): %s</p>", html.EscapeString(version), html.EscapeString(legalconsent.Text))
	}
	message := transactionalmail.Message{From: s.mail.From, To: []string{address}, Subject: "Paperboat: your Team trial has started", HTML: body}
	_, err = transactionalmail.EnqueueOnceTx(ctx, tx, "team-trial-started:"+sub.ID+":"+actor, message)
	return err
}
