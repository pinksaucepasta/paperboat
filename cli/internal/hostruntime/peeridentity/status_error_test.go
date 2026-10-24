package peeridentity

import (
	"errors"
	"fmt"
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
