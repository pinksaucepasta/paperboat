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
