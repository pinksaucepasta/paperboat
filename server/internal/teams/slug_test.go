package teams

import (
	"strings"
	"testing"
)

func TestTeamSlugHostnameBoundary(t *testing.T) {
	for _, s := range []string{"team-1", "0", "my-team", strings.Repeat("a", 63)} {
		if !validTeamSlug(s) {
			t.Fatalf("rejected %q", s)
		}
	}
	for _, s := range []string{"", "personal", "team.one", "Team", "team_one", "-team", "team-", strings.Repeat("a", 64)} {
		if validTeamSlug(s) {
			t.Fatalf("accepted %q", s)
		}
	}
	if !validID("opaque_ACCOUNT_1") {
		t.Fatal("opaque ID contract changed")
	}
}
