package billing_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
)

type recoveryStripeFake struct {
	checkout                       billing.StripeCheckoutState
	subscription                   billing.StripeSubscription
	checkoutGets, subscriptionGets int
	failSubscriptionGets           int
}

func (f *recoveryStripeFake) CreateCustomer(context.Context, string, string, string) (string, error) {
	panic("unexpected")
}
func (f *recoveryStripeFake) CreateSubscriptionCheckout(context.Context, billing.StripeCheckoutInput) (billing.CheckoutSession, error) {
	panic("unexpected")
}
func (f *recoveryStripeFake) CreatePortal(context.Context, billing.StripePortalInput) (billing.CustomerPortalSession, error) {
	panic("unexpected")
}
func (f *recoveryStripeFake) GetCheckoutSession(context.Context, string) (billing.StripeCheckoutState, error) {
	f.checkoutGets++
	return f.checkout, nil
}
func (f *recoveryStripeFake) GetSubscription(context.Context, string) (billing.StripeSubscription, error) {
	f.subscriptionGets++
	if f.failSubscriptionGets > 0 {
		f.failSubscriptionGets--
		return billing.StripeSubscription{}, context.DeadlineExceeded
	}
	return f.subscription, nil
}
func (f *recoveryStripeFake) UpdateSubscriptionSeats(context.Context, string, string, int, string) (billing.StripeSubscription, error) {
	panic("unexpected")
}

func TestWorkspaceOperatorRetiresOnlyExpiredNotAppliedOperation(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	admin, user := fmt.Sprintf("opr_%032x", time.Now().UnixNano()), "usr_recovery_"+x
	insertUser(t, store, user, user+"@example.test")
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_accounts(id,email,password_hash,role,status) VALUES($1,$1||'@example.test','unused test login','billing','active')`, admin); err != nil {
		t.Fatal(err)
	}
	team := "team_recovery_" + x
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,3)`, team, user); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, team, user); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspaces(id,team_id,provider_customer_id,provider_subscription_id,provider_item_id,state,quantity,revision) VALUES($1,$2,$3,$4,$5,'active',2,7)`, "team:"+team, team, "cus_"+x, "sub_"+x, "si_"+x); err != nil {
		t.Fatal(err)
	}
	op := "seat-op-" + x
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspace_operations(idempotency_key,workspace_id,actor_user_id,kind,request_hash,state,request_ciphertext,provider_attempted_at) VALUES($1,$2,$3,'seats',$4,'uncertain',$5,now()-interval '25 hours')`, op, "team:"+team, user, "hash-"+x, []byte("encrypted-request")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.audit_events WHERE resource_type='billing_workspace_operation' AND resource_id IN (SELECT idempotency_key FROM paperboat.billing_workspace_operations WHERE workspace_id=$1)`, "team:"+team)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspace_recoveries WHERE workspace_id=$1`, "team:"+team)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.audit_events WHERE idempotency_key=$1`, `workspace-recovery:recovery-`+x)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspace_recoveries WHERE operation_key=$1`, op)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspace_operations WHERE idempotency_key=$1`, op)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspace_operations WHERE workspace_id=$1`, "team:"+team)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspaces WHERE id=$1`, "team:"+team)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.team_members WHERE team_id=$1`, team)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.teams WHERE team_id=$1`, team)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.audit_events WHERE actor_user_id=$1`, admin)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.operator_accounts WHERE id=$1`, admin)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	s := billing.NewWorkspaceRecoveryService(store, audit.NewWriter(store), nil)
	recoveryKey := "recovery-" + x
	evidence := "stripe-search:no-object/" + x
	if err := s.RetireNotApplied(ctx, user, recoveryKey, op, evidence); !errors.Is(err, billing.ErrWorkspaceForbidden) {
		t.Fatalf("non-admin error=%v", err)
	}
	if err := s.RetireNotApplied(ctx, admin, recoveryKey, op, evidence); err != nil {
		t.Fatal(err)
	}
	if err := s.RetireNotApplied(ctx, admin, recoveryKey, op, evidence); err != nil {
		t.Fatalf("replay=%v", err)
	}
	if err := s.RetireNotApplied(ctx, admin, recoveryKey, op, "stripe-search:different/"+x); !errors.Is(err, billing.ErrBillingRecoveryConflict) {
		t.Fatalf("conflict=%v", err)
	}
	var state string
	var revision int64
	if err := store.SQL().QueryRowContext(ctx, `SELECT o.state,w.revision FROM paperboat.billing_workspace_operations o JOIN paperboat.billing_workspaces w ON w.id=o.workspace_id WHERE o.idempotency_key=$1`, op).Scan(&state, &revision); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || revision != 8 {
		t.Fatalf("state/revision=%s/%d", state, revision)
	}
	fake := &membershipStripeFake{subs: map[string]billing.StripeSubscription{"sub_" + x: {ID: "sub_" + x, CustomerID: "cus_" + x, ItemID: "si_" + x, PriceID: "price_" + x, Status: "active", Quantity: 1, PeriodStart: time.Unix(1700000000, 0), PeriodEnd: time.Unix(1702592000, 0)}}, fail: map[string]bool{}, updates: map[string]int{}}
	if err := billing.NewWorkspaceService(store, fake, "worker-recovery-key").ReconcileMemberships(ctx, 1); err != nil {
		t.Fatal(err)
	}
	var bought int
	if err := store.SQL().QueryRowContext(ctx, `SELECT quantity FROM paperboat.billing_workspaces WHERE id=$1`, "team:"+team).Scan(&bought); err != nil || bought != 2 || fake.updates["sub_"+x] != 0 {
		t.Fatalf("retiring an operation changed paid seats: quantity=%d err=%v", bought, err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.operator_accounts SET role='operations' WHERE id=$1`, admin); err != nil {
		t.Fatal(err)
	}
	if err := s.RetireNotApplied(ctx, admin, recoveryKey, op, evidence); !errors.Is(err, billing.ErrWorkspaceForbidden) {
		t.Fatalf("demoted admin replay=%v", err)
	}
	var auditCount int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE idempotency_key=$1`, `workspace-recovery:`+recoveryKey).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("audit count=%d err=%v", auditCount, err)
	}

	// A separate expired checkout is completed only from a provider session
	// whose immutable identity and line item match the stored request.
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.operator_accounts SET role='admin' WHERE id=$1`, admin); err != nil {
		t.Fatal(err)
	}
	price, product := "price_recovery_"+x, "product_recovery_"+x
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active) VALUES($1,$2,'stripe',$3,$4,'team_subscription','{}',true)`, "bp_recovery_"+x, "plan_recovery_"+x, product, price); err != nil {
		t.Fatal(err)
	}
	checkoutOp := "checkout-op-" + x
	req, _ := json.Marshal(map[string]any{"CustomerID": "cus_" + x, "PriceID": price, "PredecessorSubscriptionID": "sub_" + x, "Quantity": 2, "SuccessURL": "https://example.test/success", "CancelURL": "https://example.test/cancel"})
	cipher, err := secrets.Encrypt("operator-recovery-key", string(req))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspaces SET state='canceled' WHERE id=$1`, "team:"+team); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspace_operations(idempotency_key,workspace_id,actor_user_id,kind,request_hash,state,request_ciphertext,provider_attempted_at) VALUES($1,$2,$3,'checkout',$4,'uncertain',$5,now()-interval '25 hours')`, checkoutOp, "team:"+team, user, "checkout-hash-"+x, cipher); err != nil {
		t.Fatal(err)
	}
	f2 := &recoveryStripeFake{checkout: billing.StripeCheckoutState{ID: "cs_recovery_" + x, CustomerID: "cus_" + x, CheckoutOperation: checkoutOp, Status: "complete", Mode: "subscription", SubscriptionID: "sub_new_" + x, PriceID: price, Quantity: 2}, subscription: billing.StripeSubscription{ID: "sub_new_" + x, CustomerID: "cus_" + x, CheckoutOperation: checkoutOp, PriceID: price, ItemID: "si_new_" + x, Status: "active", Quantity: 2, PeriodStart: time.Now().Add(-time.Hour), PeriodEnd: time.Now().Add(30 * 24 * time.Hour)}}
	f2.failSubscriptionGets = 1
	recovery := billing.NewWorkspaceRecoveryService(store, audit.NewWriter(store), billing.NewWorkspaceService(store, f2, "operator-recovery-key"))
	if err = recovery.ReconcileCheckout(ctx, admin, "applied-recovery-"+x, checkoutOp, "cs_recovery_"+x, "stripe-dashboard:session/"+x); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first reconciliation error=%v", err)
	}
	if err = recovery.ReconcileCheckout(ctx, admin, "applied-recovery-"+x, checkoutOp, "cs_changed_"+x, "stripe-dashboard:session/"+x); !errors.Is(err, billing.ErrBillingRecoveryConflict) {
		t.Fatalf("changed session replay=%v", err)
	}
	if err = recovery.ReconcileCheckout(ctx, admin, "applied-recovery-"+x, checkoutOp, "cs_recovery_"+x, "stripe-dashboard:session/"+x); err != nil {
		t.Fatal(err)
	}
	var newSub, newOpState string
	if err = store.SQL().QueryRowContext(ctx, `SELECT w.provider_subscription_id,o.state FROM paperboat.billing_workspaces w JOIN paperboat.billing_workspace_operations o ON o.workspace_id=w.id WHERE w.id=$1 AND o.idempotency_key=$2`, "team:"+team, checkoutOp).Scan(&newSub, &newOpState); err != nil {
		t.Fatal(err)
	}
	if newSub != "sub_new_"+x || newOpState != "succeeded" || f2.checkoutGets != 2 || f2.subscriptionGets != 3 {
		t.Fatalf("applied recovery sub/state/gets=%s/%s/%d/%d", newSub, newOpState, f2.checkoutGets, f2.subscriptionGets)
	}
	openOp := "checkout-open-" + x
	openReq, _ := json.Marshal(map[string]any{"CustomerID": "cus_" + x, "PriceID": price, "PredecessorSubscriptionID": "sub_new_" + x, "Quantity": 2, "SuccessURL": "https://example.test/success", "CancelURL": "https://example.test/cancel"})
	openCipher, _ := secrets.Encrypt("operator-recovery-key", string(openReq))
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspace_operations(idempotency_key,workspace_id,actor_user_id,kind,request_hash,state,request_ciphertext,provider_attempted_at) VALUES($1,$2,$3,'checkout',$4,'uncertain',$5,now()-interval '25 hours')`, openOp, "team:"+team, user, "open-hash-"+x, openCipher); err != nil {
		t.Fatal(err)
	}
	f2.checkout = billing.StripeCheckoutState{ID: "cs_open_" + x, CustomerID: "cus_" + x, CheckoutOperation: openOp, Status: "open", Mode: "subscription", PriceID: price, Quantity: 2, URL: "https://checkout.stripe.test/open"}
	openRecoveryKey := "open-recovery-" + x
	if err = recovery.ReconcileCheckout(ctx, admin, openRecoveryKey, openOp, "cs_open_"+x, "stripe-dashboard:open/"+x); err != nil {
		t.Fatal(err)
	}
	f2.checkout.Status = "expired"
	if err = recovery.ReconcileCheckout(ctx, admin, openRecoveryKey, openOp, "cs_open_"+x, "stripe-dashboard:open/"+x); err != nil {
		t.Fatalf("open receipt after expiry replay=%v", err)
	}
	var openState, openDisposition string
	if err = store.SQL().QueryRowContext(ctx, `SELECT o.state,r.disposition FROM paperboat.billing_workspace_operations o JOIN paperboat.billing_workspace_recoveries r ON r.operation_key=o.idempotency_key WHERE o.idempotency_key=$1`, openOp).Scan(&openState, &openDisposition); err != nil || openState != "succeeded" || openDisposition != "recovered_checkout" {
		t.Fatalf("open recovery=%s/%s err=%v", openState, openDisposition, err)
	}
	portalOp := "portal-expired-" + x
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspace_operations(idempotency_key,workspace_id,actor_user_id,kind,request_hash,state,request_ciphertext,provider_attempted_at) VALUES($1,$2,$3,'portal',$4,'uncertain',$5,now()-interval '25 hours')`, portalOp, "team:"+team, user, "portal-hash-"+x, []byte("encrypted-request")); err != nil {
		t.Fatal(err)
	}
	if err = recovery.SupersedePortal(ctx, admin, "portal-recovery-"+x, portalOp, "stripe-dashboard:portal/"+x); err != nil {
		t.Fatal(err)
	}
	var portalState, portalDisposition string
	if err = store.SQL().QueryRowContext(ctx, `SELECT o.state,r.disposition FROM paperboat.billing_workspace_operations o JOIN paperboat.billing_workspace_recoveries r ON r.operation_key=o.idempotency_key WHERE o.idempotency_key=$1`, portalOp).Scan(&portalState, &portalDisposition); err != nil || portalState != "failed" || portalDisposition != "superseded_portal" {
		t.Fatalf("portal recovery=%s/%s err=%v", portalState, portalDisposition, err)
	}
	store.SQL().ExecContext(ctx, `DELETE FROM paperboat.audit_events WHERE idempotency_key=$1`, `workspace-recovery:applied-recovery-`+x)
	store.SQL().ExecContext(ctx, `DELETE FROM paperboat.billing_workspace_recoveries WHERE operation_key=$1`, checkoutOp)
	store.SQL().ExecContext(ctx, `DELETE FROM paperboat.billing_workspace_operations WHERE idempotency_key=$1`, checkoutOp)
	store.SQL().ExecContext(ctx, `DELETE FROM paperboat.billing_products WHERE provider_price_id=$1`, price)
}
