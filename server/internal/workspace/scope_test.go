package workspace

import (
	"context"
	"errors"
	"testing"
)

func TestParseAndContextScope(t *testing.T) {
	personal, err := Parse("personal")
	if err != nil || personal != (Scope{ID: "personal"}) {
		t.Fatalf("personal scope = %+v, %v", personal, err)
	}
	team, err := Parse("research-team")
	if err != nil || team != (Scope{ID: "research-team", TeamID: "research-team"}) {
		t.Fatalf("team scope = %+v, %v", team, err)
	}
	for _, invalid := range []string{"", "Personal", "-team", "team-", "team.slug", "team_name"} {
		if _, err := Parse(invalid); !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%q) error = %v, want ErrInvalid", invalid, err)
		}
	}

	ctx := WithContext(context.Background(), team)
	got, ok := FromContext(ctx)
	if !ok || got != team {
		t.Fatalf("context scope = %+v, %t", got, ok)
	}
}
