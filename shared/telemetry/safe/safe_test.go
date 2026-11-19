package safe

import (
	"strings"
	"testing"
)

func TestBoundedStringRedactsBoundsAndRejectsEmpty(t *testing.T) {
	got, err := BoundedString("authorization=Bearer abc customer.example.com route_customer123 ok", 64, true)
	if err != nil || strings.Contains(got, "customer") || strings.Contains(got, "abc") || !strings.Contains(got, RedactedValue) {
		t.Fatalf("BoundedString = %q, %v", got, err)
	}
	long, _ := BoundedString(strings.Repeat("界", 100), 10, true)
	if len(long) > 10 || strings.ContainsRune(long, '�') {
		t.Fatalf("not truncated on a rune boundary: %q", long)
	}
	if _, err := BoundedString("  \n", 10, true); ErrorCodeOf(err) != ErrorInvalidString {
		t.Fatalf("empty required string error = %v", err)
	}
	if _, err := BoundedString("x", 0, false); ErrorCodeOf(err) != ErrorInvalidString {
		t.Fatalf("zero bound error = %v", err)
	}
}

func TestOpaqueIDAcceptsMintedIDsAndRejectsSecretsAndHostnames(t *testing.T) {
	for _, ok := range []string{"corr_01", "5b8a43c1-574f-4c56-94e1-a1adf6fc2c83", "tun_existing_01"} {
		if !OpaqueID(ok) || !CorrelationID(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "ab", "Bearer-secret", "customer.example.com", "pbce_abc", "token_01", "a/b"} {
		if OpaqueID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestErrorCodeOfUnwrapsTypedErrors(t *testing.T) {
	err := NewError(ErrorInvalidID, "op")
	if ErrorCodeOf(err) != ErrorInvalidID || ErrorCodeOf(nil) != "" || err.Error() != "op: invalid_id" {
		t.Fatalf("ErrorCodeOf/Error = %q %q", ErrorCodeOf(err), err)
	}
}

// opaqueIDFull is OpaqueID as it was: every secret rule over the value.
func opaqueIDFull(value string) bool {
	if len(value) < 3 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "pbce_") || strings.HasPrefix(lower, "pbnative_") {
		return false
	}
	for _, marker := range []string{"bearer", "password", "passwd", "secret", "token", "credential", "private_key", "api_key", "client_secret"} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	for _, rule := range secretStringRedactions {
		if rule.Pattern.MatchString(value) {
			return false
		}
	}
	return true
}

// The guarded OpaqueID must answer exactly as running every rule did, for ordinary ids and for every token shape the rules know.
func TestOpaqueIDGuardAgreesWithEveryRule(t *testing.T) {
	cases := []string{"route_load_a12", "endpoint_load_a7", "session_load_9", "ghost_route_1", "sk_live_" + strings.Repeat("a", 16), "sk_test_" + strings.Repeat("Z9", 8),
		"ghp_" + strings.Repeat("a", 20), "gho_" + strings.Repeat("b", 25), "xoxb-" + strings.Repeat("1", 10), "AKIA" + strings.Repeat("A", 16), "ASIA" + strings.Repeat("9", 16),
		"AKIA" + strings.Repeat("A", 15), "xyz-AKIAAAAAAAAAAAAAAAA-xyz", "x_" + "sk_live_" + strings.Repeat("a", 16), "eyJ" + strings.Repeat("a", 30), "abc", "ab", ""}
	alphabet := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-"
	seed := uint64(1)
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		for n := 3 + int(seed%40); n > 0; n-- {
			seed = seed*6364136223846793005 + 1442695040888963407
			b.WriteByte(alphabet[(seed>>33)%uint64(len(alphabet))])
		}
		cases = append(cases, b.String())
	}
	for _, c := range cases {
		if got, want := OpaqueID(c), opaqueIDFull(c); got != want {
			t.Fatalf("OpaqueID(%q) = %v, every rule says %v", c, got, want)
		}
	}
}

// The rules before firstTokenRule must not match anything OpaqueID's alphabet can spell, or the guard would skip them wrongly.
func TestRulesBeforeFirstTokenRuleNeedCharactersOutsideTheAlphabet(t *testing.T) {
	probes := []string{"authorization", "Bearer", "BASIC", "token", "password", "cookie", "set_cookie", "credential_ref", "private_key", "api-key", "signed_url", "client_secret",
		"BEGINPRIVATEKEY", "authorization_token_password_secret", "-----BEGIN-PRIVATE-KEY-----END-PRIVATE-KEY-----", "Bearer-abc", "token_abc", "password-abc"}
	for _, rule := range secretStringRedactions[:firstTokenRule] {
		for _, p := range probes {
			if rule.Pattern.MatchString(p) {
				t.Fatalf("rule %s matches %q, a value of OpaqueID's alphabet", rule.Pattern, p)
			}
		}
	}
}
