package billing_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/billing"
)

// businessStripeFake implements the Stripe calls business billing makes.
type businessStripeFake struct {
	membershipStripeFake
	collection billing.Collection
	details    billing.BillingDetails
}

func (f *businessStripeFake) ListInvoices(context.Context, string, string, int) ([]billing.Invoice, bool, error) {
	return []billing.Invoice{{ID: "in_1", Status: "paid"}}, false, nil
}
func (f *businessStripeFake) GetBillingDetails(context.Context, string) (billing.BillingDetails, error) {
	return f.details, nil
}
func (f *businessStripeFake) UpdateBillingDetails(_ context.Context, _ string, in billing.BillingDetails, _ string) error {
	f.details.Email, f.details.Name, f.details.Address = in.Email, in.Name, in.Address
	return nil
}
func (f *businessStripeFake) SetTaxID(_ context.Context, _ string, taxID *billing.TaxID, _ string) error {
	f.details.TaxID = taxID
	return nil
}
func (f *businessStripeFake) GetCollection(context.Context, string) (billing.Collection, error) {
	return f.collection, nil
}
func (f *businessStripeFake) SetCollection(_ context.Context, _ string, in billing.Collection, _ string) (billing.Collection, error) {
	f.collection = in
	return in, nil
}

// PLAN F19-11/12: only the owner and billing admins manage billing; pay by invoice needs an
// active yearly Team plan with enough seats; every change is in the audit log.
func TestPostgresBusinessBillingRolesAndPayByInvoice(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	owner, admin, payer, auditor := "usr_bo_"+x, "usr_ba_"+x, "usr_bb_"+x, "usr_bau_"+x
	team, code := "biz-"+x, "team-yearly-"+x
	for _, u := range []string{owner, admin, payer, auditor} {
		insertUser(t, store, u, u+"@example.test")
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation,display_name) VALUES($1,$2,1,'Biz')`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'admin',true),($1,$4,1,'billing',true),($1,$5,1,'auditor',true)`, team, owner, admin, payer, auditor)
	exec(`INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active,verified_interval) VALUES($1,$2,'stripe',$3,$4,'team_subscription','{}',true,'year')`, "bp_"+x, code, "prod_"+x, "price_"+x)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,provider_customer_id,provider_subscription_id,provider_item_id,product_code,state,quantity,period_start,period_end) VALUES($1,$2,$3,$4,$5,$6,'active',10,now()-interval '1 day',now()+interval '364 days')`, "team:"+team, team, "cus_"+x, "sub_"+x, "si_"+x, code)
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspaces WHERE id=$1`, "team:"+team)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_products WHERE code=$1`, code)
	})
	fake := &businessStripeFake{}
	s := billing.NewWorkspaceService(store, fake, "test-key")
	for _, who := range []string{admin, auditor} {
		if _, err := s.Invoices(ctx, who, team, ""); !errors.Is(err, billing.ErrWorkspaceForbidden) {
			t.Fatalf("%s read invoices: %v", who, err)
		}
		if _, err := s.SetPayByInvoice(ctx, who, team, true, "k-"+who); !errors.Is(err, billing.ErrWorkspaceForbidden) {
			t.Fatalf("%s changed collection: %v", who, err)
		}
	}
	for _, who := range []string{owner, payer} {
		if page, err := s.Invoices(ctx, who, team, ""); err != nil || len(page.Items) != 1 {
			t.Fatalf("%s invoices=%+v err=%v", who, page, err)
		}
	}
	if _, err := s.UpdateBillingDetails(ctx, payer, team, billing.BillingDetails{Email: "nope"}, "d1"); !errors.Is(err, billing.ErrInvalidBillingDetails) {
		t.Fatalf("invalid details: %v", err)
	}
	details := billing.BillingDetails{Email: "ap@example.test", Name: "Biz GmbH", Address: billing.Address{Line1: "1 Road", City: "Berlin", PostalCode: "10115", Country: "DE"}, TaxID: &billing.TaxID{Type: "eu_vat", Value: "DE123456789"}}
	if got, err := s.UpdateBillingDetails(ctx, payer, team, details, "d2"); err != nil || got.Name != "Biz GmbH" || got.TaxID == nil {
		t.Fatalf("details=%+v err=%v", got, err)
	}
	state, err := s.SetPayByInvoice(ctx, payer, team, true, "c1")
	if err != nil || !state.PayByInvoice || state.DaysUntilDue != 30 || !fake.collection.PayByInvoice {
		t.Fatalf("pay by invoice state=%+v err=%v", state, err)
	}
	exec(`UPDATE paperboat.billing_workspaces SET quantity=9 WHERE id=$1`, "team:"+team)
	if state, err = s.PayByInvoice(ctx, owner, team); err != nil || state.Eligible || !state.PayByInvoice {
		t.Fatalf("9 seats state=%+v err=%v", state, err)
	}
	if _, err = s.SetPayByInvoice(ctx, owner, team, true, "c2"); !errors.Is(err, billing.ErrPayByInvoiceIneligible) {
		t.Fatalf("9 seats switched on: %v", err)
	}
	if state, err = s.SetPayByInvoice(ctx, owner, team, false, "c3"); err != nil || state.PayByInvoice {
		t.Fatalf("switch off state=%+v err=%v", state, err)
	}
	var events int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE resource_type='team' AND resource_id=$1 AND event_type IN ('billing.details_updated','billing.pay_by_invoice_changed')`, team).Scan(&events); err != nil || events != 3 {
		t.Fatalf("audit events=%d err=%v", events, err)
	}
}
