package peeridentity

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestStatusErrorMarksServerHiccupsTransient(t *testing.T) {
	for status, want := range map[int]bool{401: false, 403: false, 429: true, 503: true, 400: false} {
		err := fmt.Errorf("check machine endpoint approval: %w", &StatusError{Status: status})
		var refused *StatusError
		if !errors.Is(err, ErrInvalid) || !errors.As(err, &refused) || refused.Transient() != want {
			t.Fatalf("HTTP %d: transient=%v, want %v", status, refused.Transient(), want)
		}
	}
}

func TestStatusErrorNamesTheServersReason(t *testing.T) {
	body := `{"error":{"code":"invalid_certificate","message":"Machine endpoint request could not be created."}}`
	err := &StatusError{Status: 400, Code: serverErrorCode(strings.NewReader(body))}
	if err.Code != "invalid_certificate" || !strings.Contains(err.Error(), "HTTP 400 (invalid_certificate)") || !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v (code %q)", err, err.Code)
	}
	for _, bad := range []string{"", "not json", `{"error":{"code":"Has Spaces"}}`, `{"error":{"code":"` + strings.Repeat("a", 65) + `"}}`, `{"error":{}}`} {
		if code := serverErrorCode(strings.NewReader(bad)); code != "" {
			t.Fatalf("body %q gave code %q", bad, code)
		}
	}
	if got := (&StatusError{Status: 400}).Error(); got != "HTTP 400: machine endpoint enrollment is invalid" {
		t.Fatalf("no-code error = %q", got)
	}
}
