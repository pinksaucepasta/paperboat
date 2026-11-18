package legalconsent

import (
	"strings"
	"testing"
)

// The Refund Policy promises these terms; the wording must keep saying them.
func TestTextKeepsThePublishedPromises(t *testing.T) {
	for _, want := range []string{"renews automatically", "payments are not refunded", "14-day withdrawal period", "Refund Policy"} {
		if !strings.Contains(Text, want) {
			t.Errorf("consent text lost %q", want)
		}
	}
	if Version == "" || len(TextSHA256()) != 64 {
		t.Fatal("version and digest are required")
	}
}
