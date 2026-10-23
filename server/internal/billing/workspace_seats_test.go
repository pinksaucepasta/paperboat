package billing_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat-server/internal/billing"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
	"testing"
	"time"
)

func TestPurchasedSeatsSurviveMembershipReconciliation(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	user, team := "usr_seats_"+x, "seats-"+x
	insertUser(t, store, user, user+"@example.test")
	for _, q := range []string{
		`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`,
		`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`,
		`INSERT INTO paperboat.billing_workspaces(id,team_id,state,quantity,provider_subscription_id,provider_item_id,provider_customer_id) VALUES('team:'||$1,$1,'active',3,'sub_'||$1,'si_'||$1,'cus_'||$2)`,
	} {
		if _, err := store.SQL().ExecContext(ctx, q, team, user); err != nil {
			t.Fatal(err)
		}
	}
	sub := billing.StripeSubscription{ID: "sub_" + team, ItemID: "si_" + team, CustomerID: "cus_" + user, PriceID: "price_test", Status: "active", Quantity: 3, PeriodStart: time.Now().Add(-time.Hour), PeriodEnd: time.Now().Add(time.Hour)}
	fake := &membershipStripeFake{subs: map[string]billing.StripeSubscription{sub.ID: sub}, fail: map[string]bool{}, updates: map[string]int{}}
	if err := billing.NewWorkspaceService(store, fake, "seats-test-key").ReconcileMemberships(ctx, 128); err != nil {
		t.Fatal(err)
	}
	if fake.subs[sub.ID].Quantity != 3 || fake.updates[sub.ID] != 0 {
		t.Fatalf("purchased seats silently changed: quantity=%d updates=%d", fake.subs[sub.ID].Quantity, fake.updates[sub.ID])
	}
}

func TestPurchasedSeatChangesReserveInvitesAndDeferRemoval(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	owner, admin, guest, extra := "usr_so_"+x, "usr_sa_"+x, "usr_sg_"+x, "usr_sx_"+x
	team := "seat-policy-" + x
	for _, u := range []string{owner, admin, guest, extra} {
		insertUser(t, store, u, u+"@example.test")
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($1,$3,1,'admin',true)`, team, owner, admin)
	code, price := "seat-plan-"+x, "price_seat_"+x
	exec(`INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active) VALUES($1,$1,'stripe',$1,$2,'team_subscription','{}',true)`, code, price)
	start, end := time.Now().Add(-time.Hour).UTC().Truncate(time.Second), time.Now().Add(24*time.Hour).UTC().Truncate(time.Second)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,quantity,provider_subscription_id,provider_item_id,provider_customer_id,product_code,period_start,period_end) VALUES('existing:'||$1,$1,'active',2,'sub_'||$1,'si_'||$1,'cus_'||$1,$2,$3,$4)`, team, code, start, end)
	sub := billing.StripeSubscription{ID: "sub_" + team, CustomerID: "cus_" + team, ItemID: "si_" + team, PriceID: price, Status: "active", Quantity: 2, PeriodStart: start, PeriodEnd: end}
	fake := &membershipStripeFake{subs: map[string]billing.StripeSubscription{sub.ID: sub}, fail: map[string]bool{}, updates: map[string]int{}}
	service := billing.NewWorkspaceService(store, fake, "purchased-seat-test-key")
	ts := teams.NewService(store)
	state, err := ts.Get(ctx, owner, team)
	if err != nil {
		t.Fatal(err)
	}
	invite := func(op, target string) (teams.Invitation, error) {
		return ts.Invite(ctx, owner, team, teams.InviteRequest{OperationID: op + x, ExpectedGeneration: state.Generation, AccountID: target})
	}
	if _, err = invite("full", guest); !errors.Is(err, teams.ErrSeatRequired) {
		t.Fatalf("full invite error=%v", err)
	}
	if _, err = service.ChangeSeats(ctx, admin, team, 3, "admin"+x); !errors.Is(err, billing.ErrWorkspaceForbidden) {
		t.Fatalf("admin bought seats: %v", err)
	}
	quote, err := service.PreviewSeats(ctx, owner, team, 3)
	if err != nil || quote.AmountDueNow != 750 || quote.NextInvoiceAmount != 4500 || !quote.NextInvoiceAt.Equal(end) {
		t.Fatalf("preview=%+v err=%v", quote, err)
	}
	fake.fail[sub.ID] = true
	if _, err = service.ChangeSeats(ctx, owner, team, 3, "buy"+x); !errors.Is(err, billing.ErrProviderOutcomeUnknown) {
		t.Fatalf("lost response=%v", err)
	}
	if _, err = service.ChangeSeats(ctx, owner, team, 4, "second"+x); !errors.Is(err, billing.ErrCheckoutPending) {
		t.Fatalf("second purchase while uncertain=%v", err)
	}
	exec(`UPDATE paperboat.billing_workspace_operations SET provider_attempted_at=now()-interval '25 hours' WHERE idempotency_key=$1`, "buy"+x)
	if _, err = service.ChangeSeats(ctx, owner, team, 3, "buy"+x); !errors.Is(err, billing.ErrStripeOperationExpired) {
		t.Fatalf("expired safe retry window: %v", err)
	}
	if fake.updates[sub.ID] != 1 {
		t.Fatal("expired retry duplicated provider charge")
	}
	exec(`UPDATE paperboat.billing_workspace_operations SET provider_attempted_at=now() WHERE idempotency_key=$1`, "buy"+x)
	paid, err := service.ChangeSeats(ctx, owner, team, 3, "buy"+x)
	if err != nil || (paid.AmountDueNow != quote.AmountDueNow || paid.NextInvoiceAmount != quote.NextInvoiceAmount || paid.Currency != quote.Currency || !paid.NextInvoiceAt.Equal(quote.NextInvoiceAt)) {
		t.Fatalf("retry=%+v err=%v", paid, err)
	}
	if _, err = service.ChangeSeats(ctx, owner, team, 3, "buy"+x); err != nil {
		t.Fatal(err)
	}
	if fake.updates[sub.ID] != 1 {
		t.Fatalf("duplicate charge: %d", fake.updates[sub.ID])
	}
	if _, err = service.ChangeSeats(ctx, owner, team, 4, "buy"+x); !errors.Is(err, billing.ErrIdempotencyConflict) {
		t.Fatalf("key reuse=%v", err)
	}
	pending, err := invite("pending", guest)
	if err != nil {
		t.Fatal(err)
	}
	state.Generation = pending.Generation
	if _, err = invite("reserved", extra); !errors.Is(err, teams.ErrSeatRequired) {
		t.Fatalf("reservation not counted: %v", err)
	}
	if _, err = service.ChangeSeats(ctx, owner, team, 2, "too-low"+x); !errors.Is(err, billing.ErrSeatQuantity) {
		t.Fatalf("removed reserved seat: %v", err)
	}
	w, err := service.Overview(ctx, owner, team)
	if err != nil || w.Quantity != 3 || w.SeatsUsed != 3 {
		t.Fatalf("workspace=%+v err=%v", w, err)
	}
	state, err = ts.Accept(ctx, guest, pending.InvitationID, teams.AcceptRequest{OperationID: "accept" + x})
	if err != nil {
		t.Fatal(err)
	}
	w, err = service.Overview(ctx, owner, team)
	if err != nil || w.SeatsUsed != 3 || fake.updates[sub.ID] != 1 {
		t.Fatalf("accept charged twice/usage=%d err=%v", w.SeatsUsed, err)
	}
	state, err = ts.Mutate(ctx, owner, team, teams.MutationRequest{OperationID: "remove" + x, ExpectedGeneration: state.Generation, Action: "remove", AccountID: guest})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.ReconcileMemberships(ctx, 128); err != nil {
		t.Fatal(err)
	}
	w, err = service.Overview(ctx, owner, team)
	if err != nil || w.Quantity != 3 || w.SeatsUsed != 2 {
		t.Fatalf("removal lowered paid quantity: %+v %v", w, err)
	}
	reduction, err := service.ChangeSeats(ctx, owner, team, 2, "reduce"+x)
	if err != nil || reduction.AmountDueNow != 0 || reduction.NextInvoiceAmount != 3000 || !reduction.NextInvoiceAt.Equal(end) {
		t.Fatalf("reduction=%+v err=%v", reduction, err)
	}
	w, err = service.Overview(ctx, owner, team)
	if err != nil || w.Quantity != 3 || w.ScheduledQuantity == nil || *w.ScheduledQuantity != 2 {
		t.Fatalf("reduction applied early: %+v err=%v", w, err)
	}
	if _, err = invite("future-cap", extra); !errors.Is(err, teams.ErrSeatRequired) {
		t.Fatalf("reserved scheduled-away seat: %v", err)
	}
	// Cancel the reduction, reserve again, then expire the invitation: only used changes.
	if _, err = service.ChangeSeats(ctx, owner, team, 3, "cancel-reduce"+x); err != nil {
		t.Fatal(err)
	}
	pending, err = invite("expire", extra)
	if err != nil {
		t.Fatal(err)
	}
	state.Generation = pending.Generation
	exec(`UPDATE paperboat.team_invitations SET expires_at=now()-interval '1 second' WHERE invitation_id=$1`, pending.InvitationID)
	w, err = service.Overview(ctx, owner, team)
	if err != nil || w.SeatsUsed != 2 || w.Quantity != 3 {
		t.Fatalf("expiry affected paid seats: %+v %v", w, err)
	}
	pending, err = invite("cancel", guest)
	if err != nil {
		t.Fatal(err)
	}
	state.Generation = pending.Generation
	state, err = ts.CancelInvite(ctx, owner, team, pending.InvitationID, teams.MutationRequest{OperationID: "cancel-invite" + x, ExpectedGeneration: state.Generation})
	if err != nil {
		t.Fatal(err)
	}
	w, err = service.Overview(ctx, owner, team)
	if err != nil || w.SeatsUsed != 2 || w.Quantity != 3 {
		t.Fatalf("cancel affected paid seats: %+v %v", w, err)
	}
	// Concurrent invitations cannot both consume the last purchased seat.
	outcomes := make(chan error, 2)
	ready := make(chan struct{})
	for n, target := range []string{guest, extra} {
		go func(n int, target string) {
			<-ready
			_, e := ts.Invite(ctx, owner, team, teams.InviteRequest{OperationID: fmt.Sprintf("race-%d-%s", n, x), ExpectedGeneration: state.Generation, AccountID: target})
			outcomes <- e
		}(n, target)
	}
	close(ready)
	accepted := 0
	for range 2 {
		e := <-outcomes
		if e == nil {
			accepted++
		} else if !errors.Is(e, teams.ErrConflict) && !errors.Is(e, teams.ErrSeatRequired) {
			t.Fatal(e)
		}
	}
	if accepted != 1 {
		t.Fatalf("last seat admitted %d concurrent invitations", accepted)
	}
	w, err = service.Overview(ctx, owner, team)
	if err != nil || w.SeatsUsed != 3 || w.Quantity != 3 {
		t.Fatalf("concurrent reservation capacity: %+v %v", w, err)
	}

	exec(`UPDATE paperboat.team_invitations SET expires_at=now()-interval '1 second' WHERE team_id=$1 AND accepted_at IS NULL AND cancelled_at IS NULL`, team)
	if _, err = service.ChangeSeats(ctx, owner, team, 2, "renewal-reduce"+x); err != nil {
		t.Fatal(err)
	}
	// The scheduled provider phase becomes authoritative at its original renewal.
	renewed := fake.subs[sub.ID]
	renewed.Quantity = 2
	renewed.PeriodStart = end
	renewed.PeriodEnd = end.AddDate(0, 1, 0)
	fake.subs[sub.ID] = renewed
	var event billing.StripeWebhook
	event.ID = "evt_seat_renewal_" + x
	event.Type = "customer.subscription.updated"
	event.Data.Object.ID = sub.ID
	event.Data.Object.Object = "subscription"
	if _, err = service.HandleWebhook(ctx, event); err != nil {
		t.Fatal(err)
	}
	w, err = service.Overview(ctx, owner, team)
	if err != nil || w.Quantity != 2 || w.ScheduledQuantity != nil || w.ScheduledAt != nil {
		t.Fatalf("renewal did not apply/clear schedule: %+v %v", w, err)
	}

}

// A team in its free trial can buy seats (Stripe prorates the trial to $0); only a team with
// no live subscription, or one set to cancel, cannot change them.
func TestTrialingTeamCanChangeSeats(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	owner, team := "usr_tso_"+x, "seat-trial-"+x
	insertUser(t, store, owner, owner+"@example.test")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, team, owner)
	code, price := "seat-trial-plan-"+x, "price_seat_trial_"+x
	exec(`INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active) VALUES($1,$1,'stripe',$1,$2,'team_subscription','{}',true)`, code, price)
	start, end := time.Now().Add(-time.Hour).UTC().Truncate(time.Second), time.Now().Add(14*24*time.Hour).UTC().Truncate(time.Second)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,quantity,provider_subscription_id,provider_item_id,provider_customer_id,product_code,period_start,period_end) VALUES('trial:'||$1,$1,'trialing',1,'sub_'||$1,'si_'||$1,'cus_'||$1,$2,$3,$4)`, team, code, start, end)
	sub := billing.StripeSubscription{ID: "sub_" + team, CustomerID: "cus_" + team, ItemID: "si_" + team, PriceID: price, Status: "trialing", Quantity: 1, PeriodStart: start, PeriodEnd: end}
	fake := &membershipStripeFake{subs: map[string]billing.StripeSubscription{sub.ID: sub}, fail: map[string]bool{}, updates: map[string]int{}}
	service := billing.NewWorkspaceService(store, fake, "trial-seat-test-key")
	if _, err := service.PreviewSeats(ctx, owner, team, 2); err != nil {
		t.Fatalf("trialing team seat preview: %v", err)
	}
	exec(`UPDATE paperboat.billing_workspaces SET cancel_at_period_end=true WHERE team_id=$1`, team)
	if _, err := service.PreviewSeats(ctx, owner, team, 2); !errors.Is(err, billing.ErrSeatSubscription) {
		t.Fatalf("a trial set to cancel changed seats: %v", err)
	}
}
