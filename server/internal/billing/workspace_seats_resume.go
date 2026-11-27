package billing

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
)

// reapplySeatReductionOnResume keeps a scheduled seat reduction on Stripe after a cancellation is lifted (L37).
// Stripe releases a subscription's schedule when the customer cancels it, so a customer who then keeps the
// plan would renew at the old seat count although the team asked for fewer. The workspace row keeps
// scheduled_quantity through the cancellation. While the live subscription is active and not cancelling, a
// reduction recorded for this period is (re)written on Stripe when the subscription has no schedule, or when
// the row still says cancelling (a schedule left by an earlier partial attempt is rewritten; ChangeSeats
// reuses an attached schedule). It runs with no database lock held and skips while a seat operation of the
// workspace is in flight (an increase releases the schedule before it records the new count).
// A transient or unknown Stripe error is returned so the webhook stays unacknowledged and Stripe retries.
// drop reports a reduction that can no longer apply (Stripe refused it, or the team uses more seats than it
// leaves); the caller clears it. The returned snapshot carries a ScheduleID when the reduction was scheduled.
func (s *WorkspaceService) reapplySeatReductionOnResume(ctx context.Context, current StripeSubscription) (StripeSubscription, bool, error) {
	if current.CancelAtPeriodEnd || current.Status != "active" {
		return current, false, nil
	}
	provider, ok := s.stripe.(SeatProvider)
	if !ok {
		return current, false, nil
	}
	var in SeatChange
	apply, drop := false, false
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var id, team, item, customer, price string
		var cancelling, inFlight bool
		var scheduled sql.NullInt64
		var scheduledAt sql.NullTime
		err := tx.QueryRow(ctx, `SELECT w.id,coalesce(w.team_id,''),coalesce(w.provider_item_id,''),coalesce(w.provider_customer_id,''),w.cancel_at_period_end,w.scheduled_quantity,w.scheduled_quantity_at,coalesce(p.provider_price_id,''),
 EXISTS(SELECT 1 FROM billing_workspace_operations o WHERE o.workspace_id=w.id AND o.kind='seats' AND o.state IN ('pending','uncertain'))
 FROM billing_workspaces w LEFT JOIN billing_products p ON p.code=w.product_code AND p.provider='stripe' WHERE w.provider_subscription_id=$1`, current.ID).Scan(&id, &team, &item, &customer, &cancelling, &scheduled, &scheduledAt, &price, &inFlight)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if team == "" || inFlight || !scheduled.Valid || !scheduledAt.Valid || (current.ScheduleID != "" && !cancelling) {
			return nil
		}
		if int(scheduled.Int64) < 1 || !scheduledAt.Time.Equal(current.PeriodEnd) || !current.PeriodEnd.After(time.Now()) ||
			item != current.ItemID || customer != current.CustomerID || price != current.PriceID || current.Quantity <= int(scheduled.Int64) {
			// Not a reduction of this period any more; a renewal or replacement clears it in the reconcile.
			return nil
		}
		used, err := teams.SeatUsageTx(ctx, tx, team)
		if err != nil {
			return err
		}
		if used > int(scheduled.Int64) {
			drop = true
			return nil
		}
		in = SeatChange{TeamID: team, SubscriptionID: current.ID, CustomerID: current.CustomerID, ItemID: current.ItemID, PriceID: current.PriceID,
			AddonPriceID: current.AddonPriceID, Quantity: int(scheduled.Int64), PreviousQuantity: current.Quantity,
			PeriodStart: current.PeriodStart, PeriodEnd: current.PeriodEnd, ProrationAt: time.Now().Unix()}
		apply = true
		return nil
	})
	if err != nil || !apply {
		return current, drop, err
	}
	// A fresh key per attempt: ChangeSeats reuses a schedule already attached and rewrites its phases.
	key := "seat-resume:" + current.ID + ":" + strconv.FormatInt(current.PeriodEnd.Unix(), 10) + ":" + uuid.NewString()
	if _, err = provider.ChangeSeats(ctx, in, key); err != nil {
		if stripeOutcomeUncertain(err) {
			return current, false, err
		}
		// Stripe refused it for good: renewing at the current count is what will happen, so say so.
		return current, true, nil
	}
	current.ScheduleID = "reapplied"
	return current, false, nil
}
