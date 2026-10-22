package config

import (
	"testing"
	"time"
)

func TestBusinessDefaultsMatchTheUserDecisions(t *testing.T) {
	b := Default().Business
	if err := b.validate(); err != nil {
		t.Fatal(err)
	}
	if b.SSOMinSeats != 10 || b.InvoiceMinSeats != 10 || b.InvoiceDaysUntilDue != 30 || b.StripeTax ||
		b.AuditRetentionFree != 30*day || b.AuditRetentionPro != 90*day || b.AuditRetentionTeam != 365*day {
		t.Fatalf("business defaults = %+v", b)
	}
}

func TestBusinessValidationRejectsUnsafeValues(t *testing.T) {
	for name, edit := range map[string]func(*Business){
		"retention below the database guard": func(b *Business) { b.AuditRetentionFree = 7 * day },
		"pro shorter than free":              func(b *Business) { b.AuditRetentionPro = b.AuditRetentionFree - time.Hour },
		"session default above max":          func(b *Business) { b.SSOSessionDefault = b.SSOSessionMax + time.Hour },
		"no invoice due date":                func(b *Business) { b.InvoiceDaysUntilDue = 0 },
	} {
		b := defaultBusiness()
		edit(&b)
		if b.validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestStripeTaxSwitchFromEnvironment(t *testing.T) {
	c := Default()
	lookup := func(k string) (string, bool) { return "true", k == "PAPERBOAT_BILLING_STRIPE_TAX" }
	if err := overlayEnv(&c, lookup, nil); err != nil || !c.Business.StripeTax {
		t.Fatalf("stripe tax = %v, err = %v", c.Business.StripeTax, err)
	}
	bad := func(k string) (string, bool) { return "maybe", k == "PAPERBOAT_BILLING_STRIPE_TAX" }
	if err := overlayEnv(&c, bad, nil); err == nil {
		t.Fatal("invalid switch accepted")
	}
}
