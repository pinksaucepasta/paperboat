package browserterminal

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestIssueRejectsMalformedBrowserKeyBeforeAdmission(t *testing.T) {
	gateway := &Gateway{}
	for _, digest := range []string{"", strings.Repeat("A", 42), strings.Repeat("A", 43) + "="} {
		if _, _, _, err := gateway.Issue(context.Background(), "user", "session", "terminal", "https://dashboard.example.test", digest); !errors.Is(err, ErrBrowserKeyInvalid) {
			t.Fatalf("digest %q: got %v, want invalid browser key", digest, err)
		}
	}
}
