package policy

import (
	"reflect"
	"testing"
)

func TestConfigSyncRulesValidateAndPickTheFirstMatch(t *testing.T) {
	src := `{
		"tags": {"tag:prod": ["machine:db1"], "tag:ci": ["machine:runner"]},
		"config_sync": [
			{"dst": ["tag:prod"], "repository": "https://git.example.test/acme/prod.git", "branch": "live"},
			{"dst": ["machine:*"], "repository": "git@git.example.test:acme/dotfiles.git"},
		]
	}`
	d, _, errs := Validate([]byte(src), team(), Limits{})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	r, target, ok := d.ConfigSyncFor([]string{"tag:ci", "tag:prod"})
	if !ok || target != "tag:prod" || r.Repository != "https://git.example.test/acme/prod.git" || r.Branch != "live" {
		t.Fatalf("prod machine got %+v %q %v", r, target, ok)
	}
	r, target, ok = d.ConfigSyncFor(nil)
	if !ok || target != "machine:*" || r.Repository != "git@git.example.test:acme/dotfiles.git" {
		t.Fatalf("untagged machine got %+v %q %v", r, target, ok)
	}
	if _, _, ok := (Document{}).ConfigSyncFor([]string{"tag:prod"}); ok {
		t.Fatal("a policy without config_sync matched")
	}
}

func TestConfigSyncRulesRefuseBadTargetsRepositoriesAndBranches(t *testing.T) {
	src := `{
		"tags": {"tag:prod": ["machine:db1"]},
		"config_sync": [
			{"dst": ["tag:missing", "machine:db1"], "repository": "not a url"},
			{"dst": [], "repository": "https://git.example.test/x.git", "branch": "a..b"},
			{"dst": ["tag:prod"], "repository": "/srv/config.git", "mode": "push"},
			{"dst": ["tag:prod"]},
		]
	}`
	d, errs := Parse([]byte(src))
	if got, want := codes(errs), []string{"/config_sync/2/mode unknown_field", "/config_sync/3/repository required"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("parse errors\n got %q\nwant %q", got, want)
	}
	_, errs = Compile(d, team(), Limits{})
	want := []string{
		"/config_sync/0/dst/0 unknown_target",
		"/config_sync/0/dst/1 unknown_target",
		"/config_sync/0/repository invalid_repository",
		"/config_sync/1/branch invalid_branch",
		"/config_sync/1/dst required",
		"/config_sync/3/repository invalid_repository",
	}
	if got := codes(errs); !reflect.DeepEqual(got, want) {
		t.Fatalf("compile errors\n got %q\nwant %q", got, want)
	}
}

// Removing a tag drops the config_sync rules that only targeted it; repository URLs are never
// treated as references.
func TestRewriteDropsConfigSyncRulesThatLostTheirTargets(t *testing.T) {
	src := `{"config_sync": [
		{"dst": ["tag:gone"], "repository": "https://git.example.test/tag:gone.git"},
		{"dst": ["tag:gone", "tag:kept"], "repository": "tag:gone", "branch": "tag:gone"},
	]}`
	out, changed, err := Rewrite([]byte(src), nil, map[string]bool{"tag:gone": true})
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	d, errs := Parse(out)
	if len(errs) > 0 || len(d.ConfigSync) != 1 || !reflect.DeepEqual(d.ConfigSync[0], ConfigSyncRule{Dst: []string{"tag:kept"}, Repository: "tag:gone", Branch: "tag:gone"}) {
		t.Fatalf("rewritten %s: %+v %v", out, d.ConfigSync, errs)
	}
}
