package hostinstall

import (
	"errors"
	"strings"
	"testing"
)

func TestProbeErrorsKeepFirstAndLast(t *testing.T) {
	var none probeErrors
	if none.err() != nil {
		t.Fatal("no failure produced an error")
	}
	same := probeErrors{}
	same.note(errors.New("refused"))
	same.note(errors.New("refused"))
	if got := same.err().Error(); got != "refused" {
		t.Fatalf("identical failures reported as %q", got)
	}
	var different probeErrors
	first, last := errors.New("capability rejected"), errors.New("connection refused")
	different.note(first)
	different.note(nil)
	different.note(last)
	err := different.err()
	if !errors.Is(err, first) || !errors.Is(err, last) || !strings.Contains(err.Error(), "first failure") || !strings.Contains(err.Error(), "last failure") {
		t.Fatalf("both failures expected, got %v", err)
	}
}
