package httpapi

import (
	"strings"
	"testing"
)

func TestLogClientKeepsPrintableUserAgentBounded(t *testing.T) {
	if got := logClient("paperboat/dev-edcad00f"); got != "paperboat/dev-edcad00f" {
		t.Fatalf("got %q", got)
	}
	if got := logClient("a\nb\x00cé"); got != "abc" {
		t.Fatalf("control and non-ASCII bytes kept: %q", got)
	}
	if got := logClient(strings.Repeat("x", 500)); len(got) != 80 {
		t.Fatalf("length %d, want 80", len(got))
	}
}
