package billing

import (
	"context"
	"database/sql"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// The current provider snapshot decides whether a failure is still outstanding. Grace is fixed when the renewal
// first fails: seven days from the invoice's finalization, which is when Stripe makes its first automatic attempt
// (a pay-by-invoice one: its due date). It never moves for the same invoice: Stripe reports attempt_count 1 again for a
// manual retry ("manual payment attempts after the first attempt do not affect the retry schedule"), so no event field
// can tell a retry from the first failure, and an event clock would restart the grace on every manual retry (B1).
// A later unpaid invoice can only shorten it. The event is unused: replays and out-of-order deliveries all read
// the same snapshot fields, so they give the same deadline.
func updateRenewalGraceTx(ctx context.Context, tx *db.Tx, id string, current StripeSubscription, _ *StripeWebhook) error {
	failed := sql.NullTime{Time: current.RenewalFailureAt, Valid: !current.RenewalFailureAt.IsZero() && current.RenewalInvoiceID != ""}
	_, err := tx.Exec(ctx, `UPDATE billing_workspaces w SET
 renewal_grace_until=CASE WHEN $2='active' THEN NULL
 WHEN $2 IN ('past_due','unpaid') THEN CASE
 WHEN $3::timestamptz IS NULL THEN renewal_grace_until
 WHEN renewal_grace_until IS NULL THEN $3::timestamptz + make_interval(secs=>p.renewal_grace_seconds)
 WHEN renewal_grace_invoice_id=$4 THEN renewal_grace_until
 ELSE least(renewal_grace_until,$3::timestamptz + make_interval(secs=>p.renewal_grace_seconds)) END
 ELSE NULL END,
 renewal_grace_invoice_id=CASE WHEN $2='active' THEN NULL
 WHEN $2 IN ('past_due','unpaid') THEN coalesce(renewal_grace_invoice_id,NULLIF($4,'')) ELSE NULL END
FROM billing_operator_policy p WHERE w.id=$1 AND p.singleton`, id, current.Status, failed, current.RenewalInvoiceID)
	return err
}
