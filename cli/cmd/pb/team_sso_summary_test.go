package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// pb team sso status prints settings as words: on/off, "none" for no exemptions, and the
// session length in days (it printed "false", an empty list and "168h0m0s").
func TestSSOSummaryReadsAsSettings(t *testing.T) {
	var s api.TeamIdentity
	s.Seats, s.MinSeats = 10, 10
	var out bytes.Buffer
	if err := ssoSummary(&out, s); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "Require SSO: off\nJIT provisioning: off\nSSO exempt: none\n") {
		t.Fatalf("defaults:\n%s", got)
	}
	s.SSO.Required, s.SSO.SessionSeconds, s.SSO.JIT, s.SSO.Exempt = true, 7*86400, true, []string{"a@example.test"}
	out.Reset()
	if err := ssoSummary(&out, s); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "Require SSO: on, SSO session 7 days\nJIT provisioning: on\nSSO exempt: a@example.test\n") {
		t.Fatalf("configured:\n%s", got)
	}
	if got := ssoSession(36 * 3600); got != "36 hours" {
		t.Fatalf("ssoSession(36h) = %s", got)
	}
}
