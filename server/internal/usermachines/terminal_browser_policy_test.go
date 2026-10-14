package usermachines

import (
	"errors"
	"reflect"
	"testing"
)

func TestNormalizeTerminalBrowserPolicyConstrainsAudiencesAndInteraction(t *testing.T) {
	tests := []struct {
		name string
		in   TerminalBrowserAccessPolicyMutation
		want TerminalBrowserAccessPolicy
		bad  bool
	}{
		{
			name: "owner default",
			in:   TerminalBrowserAccessPolicyMutation{Audience: "owner"},
			want: TerminalBrowserAccessPolicy{TerminalSessionID: "term_1", Audience: "owner", AllowedEmails: []string{}, InteractiveEmails: []string{}},
		},
		{
			name: "exact emails normalize and sort",
			in:   TerminalBrowserAccessPolicyMutation{Audience: "specific_emails", AllowedEmails: []string{"Bee@Example.com", "alice@example.com"}, InteractiveEmails: []string{"BEE@example.com"}},
			want: TerminalBrowserAccessPolicy{TerminalSessionID: "term_1", Audience: "specific_emails", AllowedEmails: []string{"alice@example.com", "bee@example.com"}, InteractiveEmails: []string{"bee@example.com"}},
		},
		{name: "specific interactive address must be allowed", in: TerminalBrowserAccessPolicyMutation{Audience: "specific_emails", AllowedEmails: []string{"alice@example.com"}, InteractiveEmails: []string{"bee@example.com"}}, bad: true},
		{name: "duplicate case folded addresses are ambiguous", in: TerminalBrowserAccessPolicyMutation{Audience: "specific_emails", AllowedEmails: []string{"alice@example.com", "ALICE@example.com"}}, bad: true},
		{name: "wildcard domain accepted", in: TerminalBrowserAccessPolicyMutation{Audience: "specific_emails", AllowedEmails: []string{"*@Example.com"}}, want: TerminalBrowserAccessPolicy{TerminalSessionID: "term_1", Audience: "specific_emails", AllowedEmails: []string{"*@example.com"}, InteractiveEmails: []string{}}},
		{name: "subdomain wildcard rejected", in: TerminalBrowserAccessPolicyMutation{Audience: "specific_emails", AllowedEmails: []string{"*@*.example.com"}}, bad: true},
		{name: "partial local wildcard rejected", in: TerminalBrowserAccessPolicyMutation{Audience: "specific_emails", AllowedEmails: []string{"alice*@example.com"}}, bad: true},
		{name: "wildcard interactive rejected", in: TerminalBrowserAccessPolicyMutation{Audience: "specific_emails", AllowedEmails: []string{"*@example.com"}, InteractiveEmails: []string{"*@example.com"}}, bad: true},
		{name: "public terminal access is unsupported", in: TerminalBrowserAccessPolicyMutation{Audience: "public"}, bad: true},
		{name: "domain requires explicit exact interact address", in: TerminalBrowserAccessPolicyMutation{Audience: "specific_emails", AllowedEmails: []string{"*@company.com"}, InteractiveEmails: []string{"person@sub.company.com"}}, bad: true},
		{
			name: "domain and exact interactive address normalize",
			in:   TerminalBrowserAccessPolicyMutation{Audience: "specific_emails", AllowedEmails: []string{"*@Company.com"}, InteractiveEmails: []string{"Person@Company.com"}},
			want: TerminalBrowserAccessPolicy{TerminalSessionID: "term_1", Audience: "specific_emails", AllowedEmails: []string{"*@company.com"}, InteractiveEmails: []string{"person@company.com"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeTerminalBrowserPolicy("term_1", test.in)
			if test.bad {
				if !errors.Is(err, ErrTerminalBrowserPolicyInvalid) {
					t.Fatalf("expected invalid policy, got policy=%+v err=%v", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalize policy: %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("normalized policy = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestTerminalBrowserDomainRequiresExactVerifiedEmailDomain(t *testing.T) {
	for _, test := range []struct {
		email string
		want  bool
	}{
		{email: "person@company.com", want: true},
		{email: "person@sub.company.com", want: false},
		{email: "person@notcompany.com", want: false},
		{email: "@company.com", want: false},
	} {
		if got := verifiedEmailHasExactDomain(test.email, "company.com"); got != test.want {
			t.Errorf("verifiedEmailHasExactDomain(%q) = %v, want %v", test.email, got, test.want)
		}
	}
}
