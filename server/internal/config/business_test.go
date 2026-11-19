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
	if b.StripeTax ||
		b.AuditRetentionFree != 30*day || b.AuditRetentionPro != 90*day || b.AuditRetentionTeam != 365*day {
		t.Fatalf("business defaults = %+v", b)
	}
}

func TestAddonPricesAreTheUserDecision(t *testing.T) {
	b := Default().Business
	for _, c := range []struct {
		addon, interval string
		cents           int64
	}{
		{"sso", "month", 10000}, {"scim", "month", 10000}, {"sso_scim", "month", 15000},
		{"sso", "year", 100000}, {"scim", "year", 100000}, {"sso_scim", "year", 150000},
	} {
		if got, ok := b.AddonPriceCents(c.addon, c.interval); !ok || got != c.cents {
			t.Errorf("%s/%s = %d, %v; want %d", c.addon, c.interval, got, ok, c.cents)
		}
	}
	for _, c := range [][2]string{{"saml", "month"}, {"sso", "week"}, {"", "year"}} {
		if _, ok := b.AddonPriceCents(c[0], c[1]); ok {
			t.Errorf("%v accepted", c)
		}
	}
}

func TestBusinessValidationRejectsUnsafeValues(t *testing.T) {
	for name, edit := range map[string]func(*Business){
		"retention below the database guard": func(b *Business) { b.AuditRetentionFree = 7 * day },
		"pro shorter than free":              func(b *Business) { b.AuditRetentionPro = b.AuditRetentionFree - time.Hour },
		"free add-on":                        func(b *Business) { b.SSOAddonCents = 0 },
		"session default above max":          func(b *Business) { b.SSOSessionDefault = b.SSOSessionMax + time.Hour },
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
