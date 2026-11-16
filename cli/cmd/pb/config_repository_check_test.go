package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
)

// pb config repository connect reads the repository first: any Git repository with the branch
// and a commit is accepted, a broken .paperboat/config-sync.toml or a missing branch is refused
// with what to fix, and nothing is left behind in the state directory.
func TestCheckConfigRepositoryAcceptsAnyValidRepositoryAndNamesWhatIsWrong(t *testing.T) {
	state := shorttemp.Dir(t)
	check := func(endpoint, branch string) error {
		return checkConfigRepository(context.Background(), state, endpoint, "local", branch, nil)
	}
	for _, source := range []string{"", "\"version\" = 1\n"} {
		if err := check(configSyncSourceBare(t, source), "main"); err != nil {
			t.Fatalf("valid repository %q refused: %v", source, err)
		}
	}
	err := check(configSyncSourceBare(t, "mode = [\n"), "main")
	if err == nil || !strings.Contains(err.Error(), configsync.SharedSourceConfigPath) || !strings.Contains(err.Error(), "line") {
		t.Fatalf("broken manifest: %v", err)
	}
	err = check(configSyncSourceBare(t, "colour = 1\n"), "main")
	if err == nil || !strings.Contains(err.Error(), `unknown key "colour"`) {
		t.Fatalf("unknown manifest key: %v", err)
	}
	var rejection commandRejection
	if err := check(configSyncSourceBare(t, ""), "live"); !errors.As(err, &rejection) || rejection.reason != commandRejectConfigRepositoryBranch {
		t.Fatalf("missing branch: %v", err)
	}
	empty := filepath.Join(shorttemp.Dir(t), "empty.git")
	if _, err := git.PlainInit(empty, true); err != nil {
		t.Fatal(err)
	}
	if err := check(empty, "main"); err == nil {
		t.Fatal("an empty repository was accepted")
	}
	if err := checkConfigRepository(context.Background(), state, "ssh://git@git.example.test/config.git", "ssh", "main", nil); err == nil || !strings.Contains(err.Error(), "--credentials-file") {
		t.Fatalf("SSH without credentials: %v", err)
	}
	if entries, _ := os.ReadDir(state); len(entries) != 0 {
		t.Fatalf("check left %d entries in the state directory", len(entries))
	}
}

// Precedence: a machine choice (--repository, the machine file, a machine-chosen assignment)
// beats a policy rule; a rule-chosen or missing assignment follows the rule, and a rule
// repository the owner has not connected is connected with the rule's branch.
func TestConfigSyncRuleChoicePrecedenceAndConnectOnApply(t *testing.T) {
	var connected map[string]string
	rule := map[string]any{"matched": true, "team_id": "acme", "target": "tag:prod", "repository": "https://git.example.test/acme/prod.git", "branch": "live", "state": "connect_on_apply"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/machines/m1/config-rule":
			writeAPIData(t, w, rule)
		case "POST /v1/config-repositories":
			json.NewDecoder(r.Body).Decode(&connected)
			writeAPIData(t, w, map[string]any{"id": "cfgrepo_prod", "display_name": connected["display_name"], "provider": "git"})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := api.New(srv.URL, config.Credential{}, srv.Client())
	ctx := context.Background()
	choose := func(machine configsync.SourceConfig, requested string, current api.ConfigAssignment) (string, api.ConfigMachineRule) {
		t.Helper()
		id, r, err := configSyncRuleChoice(ctx, client, "m1", machine, requested, current)
		if err != nil {
			t.Fatal(err)
		}
		return id, r
	}
	// Machine choices never ask for the rule.
	if id, r := choose(configsync.SourceConfig{}, "mine", api.ConfigAssignment{}); id != "mine" || r.Matched {
		t.Fatalf("--repository: %q %+v", id, r)
	}
	if id, _ := choose(configsync.SourceConfig{Pull: &configsync.RepositoryTarget{RepositoryID: "file"}}, "", api.ConfigAssignment{}); id != "" {
		t.Fatalf("machine file: %q", id)
	}
	if id, _ := choose(configsync.SourceConfig{}, "", api.ConfigAssignment{ID: "a", ConsentState: "accepted", Source: "machine"}); id != "" {
		t.Fatalf("machine-chosen assignment: %q", id)
	}
	// No assignment: the rule decides and its repository is connected for the owner.
	id, r := choose(configsync.SourceConfig{}, "", api.ConfigAssignment{})
	if id != "cfgrepo_prod" || r.RepositoryID != "cfgrepo_prod" || connected["external_ref"] != "https://git.example.test/acme/prod.git" || connected["branch"] != "live" || connected["display_name"] != "prod" {
		t.Fatalf("connect on apply: %q %+v %v", id, r, connected)
	}
	// A rule-chosen assignment follows the current rule; a connected repository is reused.
	connected = nil
	rule["state"], rule["repository_id"] = "connected", "cfgrepo_live"
	if id, _ := choose(configsync.SourceConfig{}, "", api.ConfigAssignment{ID: "a", ConsentState: "accepted", Source: "tag_rule"}); id != "cfgrepo_live" || connected != nil {
		t.Fatalf("rule-chosen assignment: %q %v", id, connected)
	}
	rule = map[string]any{"matched": false, "state": "none"}
	if id, r := choose(configsync.SourceConfig{}, "", api.ConfigAssignment{}); id != "" || r.Matched {
		t.Fatalf("no rule: %q %+v", id, r)
	}
}
