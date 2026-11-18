package customernotices

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// The promised notice periods and the breach window are enforced by the template, not by the operator's care.
func TestRenderEnforcesThePromisedNoticeAndBreachWindow(t *testing.T) {
	for name, tc := range map[string]struct {
		in   Input
		want string // "" = valid; else a fragment of the refusal
	}{
		"price 29 days":     {Input{Kind: "price_increase", Detail: "Pro $9 to $12", Effective: "2026-12-18"}, "at least 30 days"},
		"price 30 days":     {Input{Kind: "price_increase", Detail: "Pro $9 to $12", Effective: "2026-12-19"}, ""},
		"price no detail":   {Input{Kind: "price_increase", Effective: "2027-01-30"}, "detail"},
		"terms urgent":      {Input{Kind: "terms_change", Detail: "x", Effective: "2026-11-20", Link: "https://example.test/terms", UrgentReason: "a court order"}, ""},
		"terms soon":        {Input{Kind: "terms_change", Detail: "x", Effective: "2026-11-20", Link: "https://example.test/terms"}, "at least 30 days"},
		"terms http link":   {Input{Kind: "terms_change", Detail: "x", Effective: "2027-02-01", Link: "http://example.test/terms"}, "https"},
		"terms no link":     {Input{Kind: "terms_change", Detail: "x", Effective: "2027-02-01"}, "https"},
		"subprocessor":      {Input{Kind: "subprocessor", Name: "Acme", Purpose: "send mail", Location: "EU", Contact: "privacy@example.test", Effective: "2026-12-30"}, ""},
		"subprocessor mail": {Input{Kind: "subprocessor", Name: "Acme", Purpose: "send mail", Location: "EU", Contact: "not an address", Effective: "2026-12-30"}, "email"},
		"breach":            {Input{Kind: "breach", AwareAt: "2026-11-19T10:00:00Z", What: "w", Data: "d", Actions: "a", Contact: "privacy@example.test"}, ""},
		"breach in future":  {Input{Kind: "breach", AwareAt: "2026-11-19T13:00:00Z", What: "w", Data: "d", Actions: "a", Contact: "privacy@example.test"}, "future"},
		"unknown kind":      {Input{Kind: "newsletter"}, "kind"},
		"field over 1000":   {Input{Kind: "price_increase", Detail: strings.Repeat("x", 1001), Effective: "2027-01-30"}, "1000"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, _, err := render(tc.in, now)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
		})
	}
	breach := Input{Kind: "breach", AwareAt: "2026-11-17T11:00:00Z", What: "<b>w</b>", Data: "d", Actions: "a", Contact: "privacy@example.test"}
	_, body, late, deadline, err := render(breach, now)
	if err != nil || !late || !deadline.Equal(time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC)) || strings.Contains(body, "<b>w</b>") || !strings.Contains(body, "72 hours") {
		t.Fatalf("late breach: err=%v late=%v deadline=%v body=%s", err, late, deadline, body)
	}
}
