package browseraccess

import (
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/viewerallow"
)

func TestViewerPolicyEmailAndDomainMatchingIsExact(t *testing.T) {
	allowed, ok := viewerallow.Email("Person+preview@Company.COM")
	if !ok || allowed != "person+preview@company.com" {
		t.Fatalf("viewerallow.Email returned %q, %v", allowed, ok)
	}
	unicodeDomain, ok := viewerallow.Email("person@bücher.example")
	if !ok || unicodeDomain != "person@xn--bcher-kva.example" {
		t.Fatalf("IDNA email returned %q, %v", unicodeDomain, ok)
	}
	if !viewerallow.Allows([]string{allowed}, "person+preview@company.com") || viewerallow.Allows([]string{allowed}, "person@company.com") {
		t.Fatal("specific-email audience was not exact")
	}
	for _, tc := range []struct {
		email, domain string
		want          bool
	}{
		{"Person@Company.COM", "company.com", true},
		{"Person@sub.company.com", "company.com", false},
		{"Person@notcompany.com", "company.com", false},
		{"Person@XN--BCHER-KVA.example", "bücher.example", true},
	} {
		_, addressDomain, found := splitCanonicalEmail(tc.email)
		domain, valid := viewerallow.Domain(tc.domain)
		if got := found && valid && addressDomain == domain; got != tc.want {
			t.Errorf("domain match email=%q domain=%q: got %v want %v", tc.email, tc.domain, got, tc.want)
		}
	}
}

func TestViewerPolicyEffectiveTunnelModeRequiresAuthenticatedGrant(t *testing.T) {
	for audience, want := range map[string]string{
		AudiencePublic:         "public",
		AudienceOwner:          "private",
		AudienceTeam:           "private",
		AudienceSpecificEmails: "private",
		AudienceEmailDomain:    "private",
	} {
		if got := effectiveTunnelViewerMode(audience); got != want {
			t.Errorf("effectiveTunnelViewerMode(%q) = %q, want %q", audience, got, want)
		}
	}
}

func TestViewerPolicyValidationRejectsWildcardsAndAmbiguity(t *testing.T) {
	for _, raw := range []string{
		"person*@company.com", "person@*.company.com",
		"Display Name <person@company.com>", "person@company..com", "person@company.com.",
		"person@xn--.com", "person@company.com/path", " person@company.com",
	} {
		if _, ok := viewerallow.Email(raw); ok {
			t.Errorf("viewerallow.Email accepted %q", raw)
		}
	}
	for _, raw := range []string{"*.company.com", "company.com.*", "company..com", "company.com.", "127.0.0.1", "company.com/path", "XN--.com"} {
		if _, ok := viewerallow.Domain(raw); ok {
			t.Errorf("viewerallow.Domain accepted %q", raw)
		}
	}
	policy, err := normalizeViewerPolicy(SetViewerPolicyRequest{Audience: AudienceSpecificEmails, AllowedEmails: []string{"A@Company.com", "a@company.com"}})
	if err == nil || policy.Audience != "" {
		t.Fatal("duplicate canonical emails were accepted")
	}
	policy, err = normalizeViewerPolicy(SetViewerPolicyRequest{Audience: AudienceEmailDomain, EmailDomain: "bücher.example"})
	if err != nil || policy.EmailDomain != "xn--bcher-kva.example" {
		t.Fatalf("valid IDNA domain = %#v, %v", policy, err)
	}
	if len(policy.AllowedEmails) != 0 {
		t.Fatalf("unexpected email selectors: %#v", policy.AllowedEmails)
	}
}

func TestViewerPolicyAllowListTakesAddressesAndExactDomains(t *testing.T) {
	policy, err := normalizeViewerPolicy(SetViewerPolicyRequest{Audience: AudienceSpecificEmails, AllowedEmails: []string{"User@X.com", "*@Company.com"}})
	if err != nil || strings.Join(policy.AllowedEmails, ",") != "user@x.com,*@company.com" {
		t.Fatalf("mixed allow list = %#v, %v", policy.AllowedEmails, err)
	}
	for _, bad := range [][]string{{"*"}, {"*@*"}, {"*@com"}, {"*@*.company.com"}, {"a*@company.com"}, {"user@x.com", "*"}, {"*@company.com", "*@COMPANY.com"}, make([]string, 101)} {
		if _, err := normalizeViewerPolicy(SetViewerPolicyRequest{Audience: AudienceSpecificEmails, AllowedEmails: bad}); !errors.Is(err, ErrInvalid) {
			t.Errorf("allow list %q = %v, want ErrInvalid", bad, err)
		}
	}
	// The wildcard is a list entry only; the one-domain audience takes a plain domain.
	if _, err := normalizeViewerPolicy(SetViewerPolicyRequest{Audience: AudienceEmailDomain, EmailDomain: "*@company.com"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("email_domain accepted a wildcard entry: %v", err)
	}
}

// A private resource is narrowed to its owner or to listed people. It is never made public or opened to a team.
func TestViewerPolicyNeverWidensANativeMode(t *testing.T) {
	audiences := []string{AudienceOwner, AudienceTeam, AudienceSpecificEmails, AudienceEmailDomain, AudiencePublic}
	for _, tc := range []struct {
		kind, mode string
		allowed    []string
		refusal    error
	}{
		{"tunnel", "public", audiences, nil},
		{"tunnel", "private", []string{AudienceOwner, AudienceSpecificEmails, AudienceEmailDomain}, ErrPolicyWidening},
		{"tunnel", "team", []string{AudienceOwner}, ErrPolicyWidening},
		{"tunnel", "", nil, ErrPolicyWidening},
		{"preview", "public", []string{AudiencePublic}, ErrPolicyEdgeTransition},
		{"preview", "private", []string{AudienceOwner, AudienceSpecificEmails, AudienceEmailDomain}, ErrPolicyWidening},
		{"preview", "team", []string{AudienceOwner}, ErrPolicyWidening},
	} {
		for _, audience := range audiences {
			want := tc.refusal
			for _, allowed := range tc.allowed {
				if allowed == audience {
					want = nil
				}
			}
			if err := ensurePolicyEdgeMode(tc.kind, tc.mode, audience); err != want {
				t.Errorf("%s native=%q audience=%q = %v, want %v", tc.kind, tc.mode, audience, err, want)
			}
		}
	}
}

func TestBrowserReturnPathBindsTheMatchedDecodedPath(t *testing.T) {
	for in, want := range map[string]string{
		"/":                            "/",
		"/app/?next=%2Fhome":           "/app",
		"/a%20b?x=1":                   "/a b",
		"/%2e%2e/secret":               "",
		"/double//path":                "",
		"https://outside.invalid/path": "",
	} {
		got, err := returnRequestPath(in)
		if want == "" {
			if err == nil {
				t.Errorf("returnRequestPath(%q) = %q, want error", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("returnRequestPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestRouteHostResolutionUsesExactAndOneLabelWildcardRules(t *testing.T) {
	for _, tc := range []struct {
		kind, rule, host string
		class            int
	}{
		{"exact", "preview.company.com", "preview.company.com", 3},
		{"exact", "company.com", "preview.company.com", 0},
		{"one_label_wildcard", "*.company.com", "preview.company.com", 2},
		{"one_label_wildcard", "*.company.com", "a.preview.company.com", 0},
		{"one_label_wildcard", "*.company.com", "company.com", 0},
	} {
		class, _ := hostnameMatch(tc.kind, tc.rule, tc.host)
		if class != tc.class {
			t.Errorf("hostnameMatch(%q,%q,%q) class=%d want=%d", tc.kind, tc.rule, tc.host, class, tc.class)
		}
	}
}

func splitCanonicalEmail(raw string) (string, string, bool) {
	canonical, ok := viewerallow.Email(raw)
	if !ok {
		return "", "", false
	}
	local, domain, found := strings.Cut(canonical, "@")
	return local, domain, found
}
