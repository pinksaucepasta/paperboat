package native

import (
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

// X-41: streams refused for scope ask for a fresh signed network, at most once per spacing.
func TestScopeRefusalAsksForAFreshNetwork(t *testing.T) {
	var asked int
	owner := &Owner{requestRefresh: func() { asked++ }}
	session := &Session{owner: owner}
	for range 30 {
		if err := session.refuseScope(); !errors.Is(err, tailnet.ErrAdmission) {
			t.Fatalf("refusal = %v", err)
		}
	}
	if asked != 1 {
		t.Fatalf("a storm of refusals asked %d times for a fresh network", asked)
	}
	owner.refreshRequested.Store(0)
	_ = session.refuseScope()
	if asked != 2 {
		t.Fatalf("a later refusal did not ask again: %d", asked)
	}
	if err := (&Session{owner: &Owner{}}).refuseScope(); !errors.Is(err, tailnet.ErrAdmission) {
		t.Fatalf("a session without a refresh hook: %v", err)
	}
}
