package session

import (
	"bytes"
	"errors"
	"github.com/google/uuid"
	"io"
	"strings"
	"testing"
)

func TestSessionIDUsesUUIDv4AndPropagatesEntropyFailure(t *testing.T) {
	value, err := randomID(bytes.NewReader(make([]byte, 16)))
	if err != nil {
		t.Fatal(err)
	}
	id, err := uuid.Parse(strings.TrimPrefix(value, "terminal_"))
	if err != nil || id.Version() != 4 || id.Variant() != uuid.RFC4122 || "terminal_"+id.String() != value {
		t.Fatalf("invalid session ID %q: %v", value, err)
	}
	value, err = randomID(bytes.NewReader(nil))
	if value != "" || !errors.Is(err, io.EOF) {
		t.Fatalf("entropy failure = %q, %v", value, err)
	}
}
