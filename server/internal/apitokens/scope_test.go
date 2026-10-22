package apitokens

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat/shared/policy"
)

func TestParseScopeAcceptsEntriesAndRoundTrips(t *testing.T) {
	scope, errs := ParseScope(json.RawMessage(`[{"dst":["tag:ci"],"allow":["exec","files"]},{"dst":["machine:db"],"allow":["private_access"],"ports":[5432,"8000-8100"]}]`))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if len(scope) != 2 || !slices.Equal(scope[1].Ports, []policy.PortRange{{From: 5432, To: 5432}, {From: 8000, To: 8100}}) {
		t.Fatalf("scope = %+v", scope)
	}
	stored, err := json.Marshal(scope)
	if err != nil {
		t.Fatal(err)
	}
	again, errs := ParseScope(stored)
	if len(errs) > 0 || len(again) != 2 || !slices.Equal(again[1].Ports, scope[1].Ports) {
		t.Fatalf("round trip %s: %+v %v", stored, again, errs)
	}
}

func TestParseScopeNoScope(t *testing.T) {
	for _, raw := range []string{"", "null", "  "} {
		if scope, errs := ParseScope(json.RawMessage(raw)); scope != nil || errs != nil {
			t.Fatalf("%q: %v %v", raw, scope, errs)
		}
	}
}

func TestParseScopeRejectsBadShapes(t *testing.T) {
	for raw, want := range map[string]string{
		`{}`:                                  "/scope wrong_type",
		`[]`:                                  "/scope empty",
		`[1]`:                                 "/scope/0 wrong_type",
		`[{"allow":["exec"]}]`:                "/scope/0/dst required",
		`[{"dst":[],"allow":["exec"]}]`:       "/scope/0/dst empty",
		`[{"dst":["tag:a"],"allow":[]}]`:      "/scope/0/allow empty",
		`[{"dst":["tag:a"],"allow":["fly"]}]`: "/scope/0/allow/0 unknown_action",
		`[{"dst":["tag:a"],"allow":["exec"],"x":1}]`:                     "/scope/0/x unknown_field",
		`[{"dst":["tag:a"],"allow":["exec"],"ports":[22]}]`:              "/scope/0/ports ports_need_private_access",
		`[{"dst":["tag:a"],"allow":["private_access"],"ports":[0]}]`:     "/scope/0/ports/0 invalid_port",
		`[{"dst":["tag:a"],"allow":["private_access"],"ports":["9-1"]}]`: "/scope/0/ports/0 invalid_port",
		`[{"dst":"tag:a","allow":["exec"]}]`:                             "/scope/0/dst wrong_type",
	} {
		scope, errs := ParseScope(json.RawMessage(raw))
		if scope != nil || len(errs) == 0 {
			t.Fatalf("%s: accepted", raw)
		}
		var got []string
		for _, e := range errs {
			got = append(got, e.Path+" "+e.Code)
		}
		if !slices.Contains(got, want) {
			t.Fatalf("%s: errors %v, want %q", raw, got, want)
		}
	}
}

func TestSecretFormat(t *testing.T) {
	secret, hash, err := newSecret()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, SecretPrefix) || len(secret) != len(SecretPrefix)+43 || !wellFormed(secret) {
		t.Fatalf("secret %d chars, well formed %v", len(secret), wellFormed(secret))
	}
	if len(hash) != 32 || string(hash) != string(hashSecret(secret)) {
		t.Fatal("hash is not sha256 of the secret")
	}
	for _, bad := range []string{"", "pbat_", secret[1:], secret + "A", "pbat_" + strings.Repeat("!", 43), strings.Replace(secret, "pbat_", "pbxx_", 1)} {
		if wellFormed(bad) {
			t.Fatalf("%q is well formed", bad)
		}
	}
}

func TestLifetimeUsesConfiguredBounds(t *testing.T) {
	s := New(nil, nil, config.APITokens{MaxLifetimeDays: 10, DefaultLifetimeDays: 3, MaxPerMember: 1})
	if d, err := s.lifetime(0); err != nil || d != 3*24*time.Hour {
		t.Fatalf("default: %v %v", d, err)
	}
	if d, err := s.lifetime(10); err != nil || d != 10*24*time.Hour {
		t.Fatalf("max: %v %v", d, err)
	}
	for _, days := range []int{-1, 11} {
		if _, err := s.lifetime(days); err != ErrInvalidLifetime {
			t.Fatalf("%d: %v", days, err)
		}
	}
}
