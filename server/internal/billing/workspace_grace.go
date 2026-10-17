package billing

import (
	"context"
	"database/sql"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// The current provider snapshot decides whether a failure is still outstanding.
// The first failure event refines the invoice's stable finalization clock;
// retries and later unpaid invoices cannot restart an unresolved grace period.
func updateRenewalGraceTx(ctx context.Context, tx *db.Tx, id string, current StripeSubscription, event *StripeWebhook) error {
	firstFailure := event != nil && event.Type == "invoice.payment_failed" && event.Created > 0 &&
		event.Data.Object.ID == current.RenewalInvoiceID &&
		event.Data.Object.BillingReason == "subscription_cycle" && event.Data.Object.AttemptCount == 1
	failed := sql.NullTime{Time: current.RenewalFailureAt, Valid: !current.RenewalFailureAt.IsZero() && current.RenewalInvoiceID != ""}
	if firstFailure && failed.Valid {
		failed.Time = time.Unix(event.Created, 0).UTC()
	}
	_, err := tx.Exec(ctx, `UPDATE billing_workspaces w SET
 renewal_grace_until=CASE WHEN $2 IN ('active','trialing') THEN NULL
 WHEN $2 IN ('past_due','unpaid') THEN CASE
 WHEN $3::timestamptz IS NULL THEN renewal_grace_until
 WHEN renewal_grace_until IS NULL OR ($5 AND renewal_grace_invoice_id=$4)
 THEN $3::timestamptz + make_interval(secs=>p.renewal_grace_seconds)
 WHEN renewal_grace_invoice_id=$4 THEN renewal_grace_until
 ELSE least(renewal_grace_until,$3::timestamptz + make_interval(secs=>p.renewal_grace_seconds)) END
 ELSE NULL END,
 renewal_grace_invoice_id=CASE WHEN $2 IN ('active','trialing') THEN NULL
 WHEN $2 IN ('past_due','unpaid') THEN coalesce(renewal_grace_invoice_id,NULLIF($4,'')) ELSE NULL END
FROM billing_operator_policy p WHERE w.id=$1 AND p.singleton`, id, current.Status, failed, current.RenewalInvoiceID, firstFailure)
	return err
}
