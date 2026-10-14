package controlplane

import (
	"strings"
	"testing"
)

func TestConfigRepositoryTransport(t *testing.T) {
	for _, tc := range []struct{ raw, transport string }{
		{"https://git.example.test/config.git", "https"}, {"http://192.168.1.20/config.git", "http"}, {"ssh://git@git.example.test:2222/srv/config.git", "ssh"}, {"git@host:config.git", "ssh"}, {"/srv/config.git", "local"}, {`C:\Users\owner\config.git`, "local"}, {`\\server\share\config.git`, "local"}, {"file:///srv/config.git", "local"},
	} {
		if got, ok := ConfigRepositoryTransport(tc.raw); !ok || got != tc.transport {
			t.Errorf("%q = %q %v", tc.raw, got, ok)
		}
	}
	for _, raw := range []string{"", "relative.git", "https://token@host/repo.git", "https://user:secret@host/repo.git", "ssh://git:secret@host/repo.git", "https://host/repo.git?token=secret", "ssh://host/repo.git#secret", "file://otherhost/repo.git", "https://host/../repo.git", "https://host/%2e%2e/repo.git", "/srv/../secret", "ftp://host/repo.git", "https://host/repo.git\n", "git@host:../repo.git"} {
		if _, ok := ConfigRepositoryTransport(raw); ok {
			t.Errorf("unsafe coordinate accepted: %q", raw)
		}
	}
}
func TestConfigMachineRuleValidation(t *testing.T) {
	rule := ConfigPathRule{Source: "machine", ID: "editor", RepositoryPath: "editor", LocalPath: `C:\Users\owner\AppData\Roaming\Editor`, Kind: "directory", Include: []string{"**/*.json", "settings?.[jt]son"}, Exclude: []string{"private/**"}}
	if err := ValidateConfigMachineRules([]ConfigPathRule{rule}, []ConfigRepositoryBinding{{RepositoryID: "repo", URL: "ssh://git@host/config.git"}}, "repo", ""); err != nil {
		t.Fatal(err)
	}
	for _, destination := range []string{"~/editor", "~\\editor", "$XDG_CONFIG_HOME/editor", "%APPDATA%\\Editor", "%LOCALAPPDATA%/Editor", "%USERPROFILE%/Editor", "~", "%APPDATA%"} {
		r := rule
		r.LocalPath = destination
		if err := ValidateConfigMachineRules([]ConfigPathRule{r}, nil, "repo", ""); err != nil {
			t.Fatalf("explicit user token %q rejected: %v", destination, err)
		}
	}
	for _, destination := range []string{"$HOME/editor", "~other/editor", "%UNKNOWN%/editor", "%APPDATA%foo", "~/../editor", "~/x/$OTHER"} {
		r := rule
		r.LocalPath = destination
		if ValidateConfigMachineRules([]ConfigPathRule{r}, nil, "repo", "") == nil {
			t.Fatalf("ambiguous token %q accepted", destination)
		}
	}
	if err := ValidateConfigMachineRules(nil, nil, "repo", ""); err != nil {
		t.Fatal("empty rules must be healthy no-op", err)
	}
	for _, mutate := range []func(*ConfigPathRule){func(r *ConfigPathRule) { r.LocalPath = "editor" }, func(r *ConfigPathRule) { r.RepositoryPath = "../editor" }, func(r *ConfigPathRule) { r.LocalPath = "/home/user/../root" }, func(r *ConfigPathRule) { r.Kind = "file" }, func(r *ConfigPathRule) { r.Include = []string{"["} }, func(r *ConfigPathRule) { r.Include = []string{"../*"} }, func(r *ConfigPathRule) { r.ID = "" }, func(r *ConfigPathRule) { r.Kind = "template" }} {
		r := rule
		mutate(&r)
		if ValidateConfigMachineRules([]ConfigPathRule{r}, nil, "repo", "") == nil {
			t.Errorf("invalid rule accepted: %+v", r)
		}
	}
	if ValidateConfigMachineRules([]ConfigPathRule{rule, rule}, nil, "repo", "") == nil {
		t.Fatal("duplicate rule IDs accepted")
	}
	if ValidateConfigMachineRules(nil, []ConfigRepositoryBinding{{RepositoryID: "other", URL: "/srv/repo.git"}}, "repo", "") == nil {
		t.Fatal("unassigned alias accepted")
	}
	if ValidateConfigMachineRules(nil, []ConfigRepositoryBinding{{RepositoryID: "repo", URL: "/a"}, {RepositoryID: "repo", URL: "/b"}}, "repo", "") == nil {
		t.Fatal("duplicate alias accepted")
	}
	rule.Include = []string{strings.Repeat("a", 4097)}
	if ValidateConfigMachineRules([]ConfigPathRule{rule}, nil, "repo", "") == nil {
		t.Fatal("oversized selector accepted")
	}
}
func TestConfigBranchValidation(t *testing.T) {
	for _, branch := range []string{"main", "feature/config", "settings-1"} {
		if !validConfigBranch(branch) {
			t.Fatal(branch)
		}
	}
	for _, branch := range []string{"../main", "main.lock", "main~1", "@{x}", "-main", "a//b", "a/", "a\\b"} {
		if validConfigBranch(branch) {
			t.Fatal(branch)
		}
	}
}

func TestConfigRuleSourcePriorityProjectionValidation(t *testing.T) {
	shared := ConfigPathRule{ID: "global", Source: "shared", RepositoryPath: "editor", LocalPath: "/home/user/editor-global", Kind: "directory"}
	machine := ConfigPathRule{ID: "override", Source: "machine", RepositoryPath: "editor/settings.json", LocalPath: "/home/user/settings.json", Kind: "file"}
	if err := ValidateConfigMachineRules([]ConfigPathRule{shared, machine}, nil, "r", ""); err != nil {
		t.Fatal("cross-source exception rejected", err)
	}
	if err := ValidateConfigMachineRules([]ConfigPathRule{machine, shared}, nil, "r", ""); err != nil {
		t.Fatal("projection priority depends on JSON order", err)
	}
	machine.Source = "shared"
	if ValidateConfigMachineRules([]ConfigPathRule{shared, machine}, nil, "r", "") == nil {
		t.Fatal("same-source repository overlap accepted")
	}
	machine.Source = "machine"
	machine.LocalPath = "/home/user/editor-global/settings.json"
	if ValidateConfigMachineRules([]ConfigPathRule{shared, machine}, nil, "r", "") != nil {
		t.Fatal("natural cross-source nested override rejected")
	}
	machine.LocalPath = "/home/user/editor-global/wrong-suffix.json"
	if ValidateConfigMachineRules([]ConfigPathRule{shared, machine}, nil, "r", "") == nil {
		t.Fatal("mismatched nested destination accepted")
	}
	machine.RepositoryPath = "other/settings.json"
	machine.LocalPath = "/home/user/editor-global/settings.json"
	if ValidateConfigMachineRules([]ConfigPathRule{shared, machine}, nil, "r", "") == nil {
		t.Fatal("unrelated repository destination alias accepted")
	}

	shared.Source = ""
	if ValidateConfigMachineRules([]ConfigPathRule{shared}, nil, "r", "") == nil {
		t.Fatal("missing source silently inferred")
	}
}
