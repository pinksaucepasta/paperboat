package operatorauth

import (
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix B, SHA-1 secret "12345678901234567890": the 8-digit reference values end in the six digits below.
func TestTOTPMatchesTheRFC6238Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037"} {
		if got := totpAt(secret, unix/totpPeriod); got != want {
			t.Errorf("T=%d: got %s want %s", unix, got, want)
		}
	}
}

func TestMatchTOTPWindowAndReplay(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Unix(1111111111, 0)
	step := now.Unix() / totpPeriod
	if got := matchTOTP(secret, totpAt(secret, step), now, 0); got != step {
		t.Fatalf("current step: %d want %d", got, step)
	}
	for _, offset := range []int64{-1, 1} {
		if got := matchTOTP(secret, totpAt(secret, step+offset), now, 0); got != step+offset {
			t.Errorf("one step of skew (%d) must be accepted: %d", offset, got)
		}
	}
	for _, offset := range []int64{-2, 2} {
		if got := matchTOTP(secret, totpAt(secret, step+offset), now, 0); got != 0 {
			t.Errorf("%d steps away must be refused: %d", offset, got)
		}
	}
	if got := matchTOTP(secret, totpAt(secret, step), now, step); got != 0 {
		t.Fatal("a code whose step was already used is a replay")
	}
	if got := matchTOTP(secret, totpAt(secret, step+1), now, step); got != step+1 {
		t.Fatal("the next step is not a replay")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef"} {
		if matchTOTP(secret, bad, now, 0) != 0 {
			t.Errorf("%q must not match", bad)
		}
	}
}

func TestRecoveryCodesAreNormalizedAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		code, err := newRecoveryCode()
		if err != nil || len(code) != 19 || strings.Count(code, "-") != 3 || code != strings.ToLower(code) {
			t.Fatalf("code %q err %v", code, err)
		}
		if seen[code] {
			t.Fatal("duplicate recovery code")
		}
		seen[code] = true
		if got := normalizeCode(strings.ToUpper(code) + " "); len(got) != 16 || strings.Contains(got, "-") {
			t.Fatalf("normalized %q", got)
		}
	}
	if !isTOTPCode("012345") || isTOTPCode("12a456") || isTOTPCode("1234567") {
		t.Fatal("isTOTPCode")
	}
}

func TestEnrollmentNeedsAnEncryptionKey(t *testing.T) {
	if _, err := (&Service{}).EnrollStart(t.Context(), "t", "c"); err != ErrMFAUnavailable {
		t.Fatalf("err=%v", err)
	}
}
