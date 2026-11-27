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

// reapplySeatReductionOnResume restores a scheduled seat reduction after a cancellation is lifted (L37).
// Stripe releases a subscription's schedule when the customer cancels it, so when the customer later keeps
// the plan the subscription would renew at the old seat count although the team asked for fewer. The
// workspace row keeps scheduled_quantity through the cancellation; when the live snapshot shows the cancel
// lifted, no schedule attached and the same period, the reduction is scheduled on Stripe again before the
// change is recorded. It runs with no database lock held. An error leaves the webhook unacknowledged so
// Stripe retries it. It returns the snapshot with ScheduleID set when it scheduled the reduction.
func (s *WorkspaceService) reapplySeatReductionOnResume(ctx context.Context, current StripeSubscription) (StripeSubscription, error) {
	if current.CancelAtPeriodEnd || current.ScheduleID != "" || current.Status != "active" {
		return current, nil
	}
	provider, ok := s.stripe.(SeatProvider)
	if !ok {
		return current, nil
	}
	var in SeatChange
	apply := false
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var team, item, customer, price string
		var cancelling bool
		var scheduled sql.NullInt64
		var scheduledAt sql.NullTime
		err := tx.QueryRow(ctx, `SELECT coalesce(w.team_id,''),coalesce(w.provider_item_id,''),coalesce(w.provider_customer_id,''),w.cancel_at_period_end,w.scheduled_quantity,w.scheduled_quantity_at,coalesce(p.provider_price_id,'')
 FROM billing_workspaces w LEFT JOIN billing_products p ON p.code=w.product_code AND p.provider='stripe' WHERE w.provider_subscription_id=$1`, current.ID).Scan(&team, &item, &customer, &cancelling, &scheduled, &scheduledAt, &price)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if team == "" || !cancelling || !scheduled.Valid || !scheduledAt.Valid || int(scheduled.Int64) < 1 ||
			!scheduledAt.Time.Equal(current.PeriodEnd) || !current.PeriodEnd.After(time.Now()) ||
			item != current.ItemID || customer != current.CustomerID || price != current.PriceID || current.Quantity <= int(scheduled.Int64) {
			return nil
		}
		used, err := teams.SeatUsageTx(ctx, tx, team)
		if err != nil {
			return err
		}
		if used > int(scheduled.Int64) {
			// The team uses more seats than the reduction leaves; renewing at the current count is right.
			return nil
		}
		in = SeatChange{TeamID: team, SubscriptionID: current.ID, CustomerID: current.CustomerID, ItemID: current.ItemID, PriceID: current.PriceID,
			AddonPriceID: current.AddonPriceID, Quantity: int(scheduled.Int64), PreviousQuantity: current.Quantity,
			PeriodStart: current.PeriodStart, PeriodEnd: current.PeriodEnd, ProrationAt: time.Now().Unix()}
		apply = true
		return nil
	})
	if err != nil || !apply {
		return current, err
	}
	// A fresh key per attempt: a retry after a partial failure finds the schedule it created and updates it.
	key := "seat-resume:" + current.ID + ":" + strconv.FormatInt(current.PeriodEnd.Unix(), 10) + ":" + uuid.NewString()
	if _, err = provider.ChangeSeats(ctx, in, key); err != nil {
		return current, err
	}
	current.ScheduleID = "reapplied"
	return current, nil
}
