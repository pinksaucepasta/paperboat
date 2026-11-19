package config

import (
	"errors"
	"time"
)

// Business holds every changeable value of the Team plan's business features (PLAN F19):
// SSO and directory sync, the audit log and business billing. One place; nothing is copied.
type Business struct {
	// The Team plan's SSO and SCIM add-ons, in US cents per month; a year costs AddonYearlyMonths months. These are the only
	// place the prices are written: billing refuses a Stripe price that differs, and the marketing check reads them here.
	SSOAddonCents     int `json:"sso_addon_cents"`
	SCIMAddonCents    int `json:"scim_addon_cents"`
	SSOSCIMAddonCents int `json:"sso_scim_addon_cents"`
	AddonYearlyMonths int `json:"addon_yearly_months"`
	// SSOSession* bound a team's "SSO session length": how long an SSO sign-in keeps the team open.
	SSOSessionDefault time.Duration `json:"sso_session_default"`
	SSOSessionMin     time.Duration `json:"sso_session_min"`
	SSOSessionMax     time.Duration `json:"sso_session_max"`
	SSOExemptMax      int           `json:"sso_exempt_max"`
	// WorkOSEventPoll is how often the WorkOS Events API is read for SSO and directory changes.
	WorkOSEventPoll time.Duration `json:"workos_event_poll"`

	AuditRetentionFree time.Duration `json:"audit_retention_free"`
	AuditRetentionPro  time.Duration `json:"audit_retention_pro"`
	AuditRetentionTeam time.Duration `json:"audit_retention_team"`
	AuditExportMaxRows int           `json:"audit_export_max_rows"`
	// AuditStream* bound delivery to a customer's SIEM destination.
	AuditStreamBatchBytes  int           `json:"audit_stream_batch_bytes"`
	AuditStreamBatchWindow time.Duration `json:"audit_stream_batch_window"`
	AuditStreamMaxBackoff  time.Duration `json:"audit_stream_max_backoff"`
	AuditStreamAlertAfter  time.Duration `json:"audit_stream_alert_after"`

	// StripeTax turns on Stripe Tax calculation; the user flips it once tax registrations exist.
	StripeTax bool `json:"stripe_tax"`
}

// AddonPriceCents is the price of a team add-on (sso, scim or sso_scim) for one interval (month or year); ok is false for an
// unknown add-on or interval.
func (b Business) AddonPriceCents(addon, interval string) (cents int64, ok bool) {
	monthly := map[string]int{"sso": b.SSOAddonCents, "scim": b.SCIMAddonCents, "sso_scim": b.SSOSCIMAddonCents}[addon]
	switch {
	case monthly == 0:
		return 0, false
	case interval == "month":
		return int64(monthly), true
	case interval == "year":
		return int64(monthly) * int64(b.AddonYearlyMonths), true
	}
	return 0, false
}

const day = 24 * time.Hour

func defaultBusiness() Business {
	return Business{
		SSOAddonCents: 10000, SCIMAddonCents: 10000, SSOSCIMAddonCents: 15000, AddonYearlyMonths: 10,
		SSOSessionDefault: 7 * day, SSOSessionMin: day, SSOSessionMax: 30 * day, SSOExemptMax: 5,
		WorkOSEventPoll:    5 * time.Second,
		AuditRetentionFree: 30 * day, AuditRetentionPro: 90 * day, AuditRetentionTeam: 365 * day,
		AuditExportMaxRows:    1_000_000,
		AuditStreamBatchBytes: 1 << 20, AuditStreamBatchWindow: 5 * time.Second,
		AuditStreamMaxBackoff: time.Hour, AuditStreamAlertAfter: time.Hour,
	}
}

// minimumAuditRetention matches the database guard in reject_audit_event_mutation: nothing
// younger than this can be deleted, so no plan may keep events for less.
const minimumAuditRetention = 30 * day

func (b Business) validate() error {
	var errs []error
	if b.SSOExemptMax < 0 {
		errs = append(errs, errors.New("business SSO exemption limit must not be negative"))
	}
	if b.SSOAddonCents < 1 || b.SCIMAddonCents < 1 || b.SSOSCIMAddonCents < 1 || b.AddonYearlyMonths < 1 {
		errs = append(errs, errors.New("business SSO and SCIM add-on prices must be positive"))
	}
	if b.SSOSessionMin < time.Hour || b.SSOSessionMin > b.SSOSessionDefault || b.SSOSessionDefault > b.SSOSessionMax {
		errs = append(errs, errors.New("business SSO session lengths must satisfy 1h <= min <= default <= max"))
	}
	if b.WorkOSEventPoll < time.Second {
		errs = append(errs, errors.New("business WorkOS event poll must be at least one second"))
	}
	if b.AuditRetentionFree < minimumAuditRetention || b.AuditRetentionPro < b.AuditRetentionFree || b.AuditRetentionTeam < b.AuditRetentionPro {
		errs = append(errs, errors.New("business audit retention must be at least 30 days and grow Free <= Pro <= Team"))
	}
	if b.AuditExportMaxRows < 1 || b.AuditStreamBatchBytes < 1<<10 || b.AuditStreamBatchWindow < time.Second || b.AuditStreamMaxBackoff < time.Minute || b.AuditStreamAlertAfter < time.Minute {
		errs = append(errs, errors.New("business audit export and stream bounds are too small"))
	}
	return errors.Join(errs...)
}
