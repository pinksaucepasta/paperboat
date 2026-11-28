package browseraccess

import (
	"errors"
	"testing"
)

// Previews and HTTP tunnels alike: a gated (Free, managed edge) public route takes any sign-in audience but never
// "public"; an ungated (paid, self-hosted) one keeps exactly the rules it had, so a paid preview may stay public.
func TestNavigationGatePolicyRulesForPreviewsAndTunnels(t *testing.T) {
	for _, kind := range []string{"preview", "tunnel"} {
		if err := ensurePolicyEdgeMode(kind, "public", AudiencePublic, true); !errors.Is(err, ErrPlanBrowserPublic) {
			t.Fatalf("%s gated public: %v", kind, err)
		}
		for _, audience := range []string{AudienceOwner, AudienceSpecificEmails, AudienceEmailDomain} {
			if err := ensurePolicyEdgeMode(kind, "public", audience, true); err != nil {
				t.Fatalf("%s gated %s: %v", kind, audience, err)
			}
		}
		if err := ensurePolicyEdgeMode(kind, "public", AudiencePublic, false); err != nil {
			t.Fatalf("%s paid public: %v", kind, err)
		}
	}
	// Ungated behaviour unchanged: a public preview still cannot take a restricted audience, a team one cannot widen.
	if err := ensurePolicyEdgeMode("preview", "public", AudienceOwner, false); !errors.Is(err, ErrPolicyEdgeTransition) {
		t.Fatalf("paid preview owner: %v", err)
	}
	if err := ensurePolicyEdgeMode("tunnel", "team", AudienceSpecificEmails, false); !errors.Is(err, ErrPolicyWidening) {
		t.Fatalf("team tunnel widening: %v", err)
	}
}
