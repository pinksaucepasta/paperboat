package main

import (
	"context"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestConfigSyncMenuFollowsAuthoritativeAssignment(t *testing.T) {
	repo := "repo"
	for _, tc := range []struct {
		name          string
		assignment    api.ConfigAssignment
		wantConfigure string
		disable       bool
	}{
		{"disabled", api.ConfigAssignment{}, "Enable sync", false},
		{"pull", api.ConfigAssignment{PullRepositoryID: &repo, Mode: "pull_only"}, "Configure sync", true},
		{"push", api.ConfigAssignment{PushRepositoryID: &repo, Mode: "push_only"}, "Configure sync", true},
		{"legacy repository field", api.ConfigAssignment{RepositoryID: &repo, Mode: "bidirectional"}, "Configure sync", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := configSyncMenuItems(tc.assignment)
			found := map[string]string{}
			for _, item := range items {
				found[item.ID] = item.Title
			}
			if found["configure"] != tc.wantConfigure || (found["disable"] != "") != tc.disable {
				t.Fatalf("menu does not match assignment: %+v", items)
			}
			if found["status"] == "" || found["repositories"] == "" {
				t.Fatal("status or repository onboarding unavailable")
			}
		})
	}
}

func TestGitHubConnectionPollingTerminalStates(t *testing.T) {
	for _, tc := range []struct {
		state, id string
		success   bool
	}{
		{"completed", "link", true}, {"failed", "link", false}, {"expired", "link", false}, {"canceled", "link", false}, {"unknown", "link", false}, {"completed", "other", false},
	} {
		t.Run(tc.state+tc.id, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]any{"data": api.GitHubNativeLink{ID: tc.id, State: tc.state}})
			}))
			defer server.Close()
			client := api.New(server.URL, config.Credential{AccessToken: "native"}, nil)
			err := waitForGitHubNativeLink(context.Background(), client, api.GitHubNativeLink{ID: "link"})
			if (err == nil) != tc.success {
				t.Fatalf("terminal state result=%v", err)
			}
		})
	}
}

func TestGitHubConnectionPollingCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": api.GitHubNativeLink{ID: "link", State: "pending"}})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := waitForGitHubNativeLink(ctx, api.New(server.URL, config.Credential{AccessToken: "native"}, nil), api.GitHubNativeLink{ID: "link", PollIntervalSeconds: 2})
	if err == nil {
		t.Fatal("pending authorization ignored cancellation")
	}
}
