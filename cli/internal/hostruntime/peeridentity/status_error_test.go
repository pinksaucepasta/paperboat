package peeridentity

import (
	"errors"
	"fmt"
	"testing"
)

func TestStatusErrorSeparatesRejectedFromTransient(t *testing.T) {
	for status, want := range map[int][2]bool{401: {false, true}, 403: {false, true}, 429: {true, false}, 503: {true, false}, 400: {false, false}} {
		err := fmt.Errorf("check machine endpoint approval: %w", &StatusError{Status: status})
		var refused *StatusError
		if !errors.Is(err, ErrInvalid) || !errors.As(err, &refused) || refused.Transient() != want[0] || refused.Rejected() != want[1] {
			t.Fatalf("HTTP %d: transient=%v rejected=%v, want %v", status, refused.Transient(), refused.Rejected(), want)
		}
	}
}
