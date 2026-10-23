package billing_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
)

type membershipStripeFake struct {
	mu      sync.Mutex
	subs    map[string]billing.StripeSubscription
	fail    map[string]bool
	updates map[string]int
	results map[string]billing.SeatPreview
	// probe runs at the start of every provider call (outside f.mu); seat tests use it to prove the
	// call is made without holding the team write lock.
	probe  func() error
	probed []error
}

func (f *membershipStripeFake) runProbe() {
	if f.probe != nil {
		err := f.probe()
		f.mu.Lock()
		f.probed = append(f.probed, err)
		f.mu.Unlock()
	}
}

func (f *membershipStripeFake) CreateCustomer(context.Context, string, string, string) (string, error) {
	panic("unexpected")
}
func (f *membershipStripeFake) CreateSubscriptionCheckout(context.Context, billing.StripeCheckoutInput) (billing.CheckoutSession, error) {
	panic("unexpected")
}
func (f *membershipStripeFake) CreatePortal(context.Context, billing.StripePortalInput) (billing.CustomerPortalSession, error) {
	panic("unexpected")
}
func (f *membershipStripeFake) GetCheckoutSession(context.Context, string) (billing.StripeCheckoutState, error) {
	panic("unexpected")
}
func (f *membershipStripeFake) GetSubscription(_ context.Context, id string) (billing.StripeSubscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subs[id], nil
}
func (f *membershipStripeFake) UpdateSubscriptionSeats(_ context.Context, id, item string, q int, key string) (billing.StripeSubscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.subs[id]
	s.Quantity = q
	f.subs[id] = s
	f.updates[id]++
	if f.fail[id] {
		delete(f.fail, id)
		return billing.StripeSubscription{}, context.DeadlineExceeded
	}
	return s, nil
}

func (f *membershipStripeFake) PreviewSeats(_ context.Context, in billing.SeatChange) (billing.SeatPreview, error) {
	f.runProbe()
	amount := int64(0)
	if in.Quantity > in.PreviousQuantity {
		amount = int64(in.Quantity-in.PreviousQuantity) * 750
	}
	return billing.SeatPreview{AmountDueNow: amount, Currency: "usd", NextInvoiceAmount: int64(in.Quantity) * 1500, NextInvoiceAt: in.PeriodEnd}, nil
}
func (f *membershipStripeFake) ChangeSeats(_ context.Context, in billing.SeatChange, key string) (billing.SeatPreview, error) {
	f.runProbe()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.results == nil {
		f.results = map[string]billing.SeatPreview{}
	}
	if out, ok := f.results[key]; ok {
		return out, nil
	}
	out := in.Preview
	f.results[key] = out
	f.updates[in.SubscriptionID]++
	if in.Quantity >= in.PreviousQuantity {
		sub := f.subs[in.SubscriptionID]
		sub.Quantity = in.Quantity
		f.subs[in.SubscriptionID] = sub
	}
	if f.fail[in.SubscriptionID] {
		delete(f.fail, in.SubscriptionID)
		return billing.SeatPreview{}, context.DeadlineExceeded
	}
	return out, nil
}

func TestWorkspaceMembershipRecoveryAcrossTeamsAndOwnerChange(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	ownerA, ownerB, shared := "usr_oa_"+x, "usr_ob_"+x, "usr_shared_"+x
	teamA, teamB := "team-a-"+x, "team-b-"+x
	for _, u := range []string{ownerA, ownerB, shared} {
		insertUser(t, store, u, u+"@example.test")
	}
	ts := teams.NewService(store)
	createJoin := func(owner, team string) teams.Team {
		state, err := ts.Create(ctx, owner, teams.CreateRequest{OperationID: "create_" + team, TeamID: team})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspaces(id,team_id,state,quantity,period_start,period_end) VALUES($1,$2,'active',2,now()-interval '1 hour',now()+interval '1 day')`, "team:"+team, team); err != nil {
			t.Fatal(err)
		}
		state, err = ts.Get(ctx, owner, team)
		if err != nil {
			t.Fatal(err)
		}
		invite, err := ts.Invite(ctx, owner, team, teams.InviteRequest{OperationID: "invite_" + team, ExpectedGeneration: state.Generation, AccountID: shared})
		if err != nil {
			t.Fatal(err)
		}
		state, err = ts.Accept(ctx, shared, invite.InvitationID, teams.AcceptRequest{OperationID: "accept_" + team})
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	stateA := createJoin(ownerA, teamA)
	_ = createJoin(ownerB, teamB)
	price := "price_team_" + x
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active) VALUES($1,$2,'stripe',$3,$4,'team_subscription','{}',true)`, "bp_team_"+x, "team-plan-"+x, "prod_team_"+x, price); err != nil {
		t.Fatal(err)
	}
	periodStart, periodEnd := time.Now().Add(-time.Hour).UTC().Truncate(time.Second), time.Now().Add(24*time.Hour).UTC().Truncate(time.Second)
	fake := &membershipStripeFake{subs: map[string]billing.StripeSubscription{}, fail: map[string]bool{}, updates: map[string]int{}}
	for _, v := range []struct{ team, sub, item, customer string }{{teamA, "sub_a_" + x, "si_a_" + x, "cus_a_" + x}, {teamB, "sub_b_" + x, "si_b_" + x, "cus_b_" + x}} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspaces(id,team_id,provider_customer_id,provider_subscription_id,provider_item_id,product_code,state,quantity,period_start,period_end) VALUES($1,$2,$3,$4,$5,$6,'active',2,$7,$8) ON CONFLICT(id) DO UPDATE SET provider_customer_id=excluded.provider_customer_id,provider_subscription_id=excluded.provider_subscription_id,provider_item_id=excluded.provider_item_id,product_code=excluded.product_code,period_start=excluded.period_start,period_end=excluded.period_end`, "team:"+v.team, v.team, v.customer, v.sub, v.item, "team-plan-"+x, periodStart, periodEnd); err != nil {
			t.Fatal(err)
		}
		fake.subs[v.sub] = billing.StripeSubscription{ID: v.sub, CustomerID: v.customer, PriceID: price, ItemID: v.item, Status: "active", Quantity: 2, PeriodStart: periodStart, PeriodEnd: periodEnd}
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspaces(id,user_id,state,quantity) VALUES($1,$2,'active',7)`, "personal:"+shared, shared); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspace_operations WHERE workspace_id IN($1,$2)`, "team:"+teamA, "team:"+teamB)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspaces WHERE id IN($1,$2,$3)`, "team:"+teamA, "team:"+teamB, "personal:"+shared)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_products WHERE code=$1`, "team-plan-"+x)
		for _, u := range []string{ownerA, ownerB, shared} {
			store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.team_operations WHERE account_id=$1`, u)
		}
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.team_invitations WHERE team_id IN($1,$2)`, teamA, teamB)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.team_members WHERE team_id IN($1,$2)`, teamA, teamB)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.teams WHERE team_id IN($1,$2)`, teamA, teamB)
		for _, u := range []string{ownerA, ownerB, shared} {
			store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, u)
		}
	})
	service := billing.NewWorkspaceService(store, fake, "membership-test-key")
	fake.fail["sub_a_"+x] = true
	if _, err := service.ChangeSeats(ctx, ownerA, teamA, 3, "seat-timeout-"+x); !errors.Is(err, billing.ErrProviderOutcomeUnknown) {
		t.Fatalf("timeout error=%v", err)
	}
	stateA, err := ts.Mutate(ctx, ownerA, teamA, teams.MutationRequest{OperationID: "transfer_" + x, Action: "transfer", ExpectedGeneration: stateA.Generation, AccountID: shared})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ts.Mutate(ctx, ownerA, teamA, teams.MutationRequest{OperationID: "leave_" + x, Action: "leave", ExpectedGeneration: stateA.Generation}); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Portal(ctx, ownerA, teamA, "denied-"+x, "https://example.test"); !errors.Is(err, billing.ErrWorkspaceForbidden) {
		t.Fatalf("former owner portal error=%v", err)
	}
	if err = service.ReconcileMemberships(ctx, 128); err != nil {
		t.Fatal(err)
	}
	if err = service.ReconcileMemberships(ctx, 128); err != nil {
		t.Fatal(err)
	}
	if fake.subs["sub_a_"+x].Quantity != 3 || fake.subs["sub_b_"+x].Quantity != 2 || fake.updates["sub_a_"+x] != 1 || fake.updates["sub_b_"+x] != 0 {
		t.Fatalf("provider quantities/updates a=%d/%d b=%d/%d", fake.subs["sub_a_"+x].Quantity, fake.updates["sub_a_"+x], fake.subs["sub_b_"+x].Quantity, fake.updates["sub_b_"+x])
	}
	var personal int
	if err = store.SQL().QueryRowContext(ctx, `SELECT quantity FROM paperboat.billing_workspaces WHERE id=$1`, "personal:"+shared).Scan(&personal); err != nil || personal != 7 {
		t.Fatalf("personal quantity=%d err=%v", personal, err)
	}
}
