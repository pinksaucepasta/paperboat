package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

func TestTeamDefaultNeedsAGitHubRepositoryAndSaysSo(t *testing.T) {
	if err := requireGitHubTeamDefault(api.ConfigRepository{Provider: "github"}); err != nil {
		t.Fatal(err)
	}
	err := requireGitHubTeamDefault(api.ConfigRepository{Provider: "git", DisplayName: "custom"})
	if err == nil || !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "GitHub repository") {
		t.Fatalf("custom repository error = %v, want a usage refusal naming GitHub", err)
	}
}
