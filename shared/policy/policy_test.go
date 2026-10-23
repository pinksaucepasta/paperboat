package policy

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// team is a team workspace: ana owns it, bo is an admin, cy a member. db1 and runner are
// team machines, web1 is cy's personal machine shared into the team, p1 a preview on web1.
func team() Inventory {
	return Inventory{
		Team:   true,
		TeamID: "acme",
		Members: []Member{
			{AccountID: "ana", Email: "ana@example.test", Role: "owner"},
			{AccountID: "bo", Email: "bo@example.test", Role: "admin"},
			{AccountID: "cy", Email: "Cy@example.test", Role: "member"},
		},
		Machines: []Machine{
			{ID: "m-db1", Alias: "db1"},
			{ID: "m-runner", Alias: "runner"},
			{ID: "m-web1", Alias: "web1", PersonalOwner: "cy"},
		},
		Resources: []Resource{{Kind: "preview", ID: "pv-1", Name: "p1", MachineID: "m-web1", Owner: "cy"}},
	}
}

func personal() Inventory {
	return Inventory{
		Members:  []Member{{AccountID: "ana", Email: "ana@example.test", Role: "owner"}},
		Machines: []Machine{{ID: "m-laptop", Alias: "laptop", PersonalOwner: "ana"}, {ID: "m-ci", Alias: "ci", PersonalOwner: "ana"}},
	}
}

func mustCompile(t *testing.T, src string, inv Inventory) Compiled {
	t.Helper()
	_, c, errs := Validate([]byte(src), inv, Limits{})
	if len(errs) > 0 {
		t.Fatalf("Validate: %v", errs)
	}
	return c
}

func machine(id string) Target { return Target{Kind: "machine", ID: id} }

func codes(errs Errors) []string {
	var out []string
	for _, e := range errs {
		out = append(out, e.Path+" "+e.Code)
	}
	return out
}

func TestParseReportsShapeErrorsWithPaths(t *testing.T) {
	src := `{
		// comments and trailing commas are fine
		"rules": [
			{"src": ["role:member"], "dst": "machine:db1", "allow": ["exec"], "extra": 1,},
			{"src": [], "dst": [], "allow": ["exec"], "deny": ["exec"]},
			{"src": ["*"]},
			{"src": ["*"], "dst": ["tag:x"], "allow": ["private_access"], "ports": [0, "9-3", 22, "80-90"]},
		],
		"tags": {"tag:a": [], "tag:a": []},
		"surprise": true,
	}`
	_, errs := Parse([]byte(src))
	want := []string{
		"/rules/0/dst wrong_type",
		"/rules/0/extra unknown_field",
		"/rules/1 allow_or_deny",
		"/rules/2 allow_or_deny",
		"/rules/2/dst required",
		"/rules/3/ports/0 invalid_port",
		"/rules/3/ports/1 invalid_port",
		"/surprise unknown_field",
		"/tags/tag:a duplicate_key",
	}
	if got := codes(errs); !reflect.DeepEqual(got, want) {
		t.Fatalf("errors\n got %q\nwant %q", got, want)
	}
	_, again := Parse([]byte(src))
	if !reflect.DeepEqual(errs, again) {
		t.Fatal("the same file gave a different error list")
	}
}

func TestParseRejectsSyntax(t *testing.T) {
	_, errs := Parse([]byte(`{"rules": [`))
	if len(errs) != 1 || errs[0].Code != "syntax" || errs[0].Path != "" {
		t.Fatalf("got %v", errs)
	}
}

func TestCompileChecksNamesAgainstTheWorkspace(t *testing.T) {
	src := `{
		"groups": {"group:ops": ["user:nobody@example.test"], "oncall": []},
		"tags": {"tag:prod": ["machine:db9", "machine:*"]},
		"rules": [
			{"src": ["group:nope", "role:boss", "bo@example.test"], "dst": ["tag:missing"], "allow": ["exec"]},
			{"src": ["*"], "dst": ["machine:db1", "env:team"], "allow": ["exec"]},
			{"src": ["*"], "dst": ["preview:p1"], "allow": ["terminal", "teleport"]},
			{"src": ["*"], "dst": ["machine:db1"], "allow": ["exec"], "ports": [22]},
		]
	}`
	d, errs := Parse([]byte(src))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	_, errs = Compile(d, team(), Limits{})
	want := []string{
		"/groups/group:ops/0 unknown_user",
		"/groups/oncall invalid_name",
		"/rules/0/dst/0 unknown_tag",
		"/rules/0/src/0 unknown_group",
		"/rules/0/src/1 unknown_role",
		"/rules/0/src/2 invalid_subject",
		"/rules/1/dst/1 mixed_targets",
		"/rules/2/allow/0 action_not_for_target",
		"/rules/2/allow/1 unknown_action",
		"/rules/3/ports ports_need_private_access",
		"/tags/tag:prod/0 unknown_machine",
		"/tags/tag:prod/1 not_allowed_in_tag",
	}
	if got := codes(errs); !reflect.DeepEqual(got, want) {
		t.Fatalf("errors\n got %q\nwant %q", got, want)
	}
}

func TestTeamOnlyFieldsAreRefusedInAPersonalWorkspace(t *testing.T) {
	d, _ := Parse([]byte(`{"allow_private_access": true, "rules": [{"src": ["*"], "dst": ["env:team"], "allow": ["env_read"]}]}`))
	_, errs := Compile(d, personal(), Limits{})
	if got := codes(errs); !reflect.DeepEqual(got, []string{"/allow_private_access team_only", "/rules/0/dst/0 team_only"}) {
		t.Fatalf("got %q", got)
	}
}

func TestLimits(t *testing.T) {
	_, _, errs := Validate([]byte(`{"rules": []}`), team(), Limits{MaxBytes: 4})
	if len(errs) != 1 || errs[0].Code != "too_large" {
		t.Fatalf("got %v", errs)
	}
	d, _ := Parse([]byte(`{"tags": {"tag:a": ["machine:db1", "machine:runner"]}, "rules": [{"src": ["*"], "dst": ["tag:a"], "allow": ["exec"]}, {"src": ["*"], "dst": ["tag:a"], "allow": ["files"]}]}`))
	_, errs = Compile(d, team(), Limits{MaxRules: 1, MaxEntriesPerList: 1})
	if got := codes(errs); !reflect.DeepEqual(got, []string{"/rules limit", "/tags/tag:a limit"}) {
		t.Fatalf("got %q", got)
	}
}

func TestPlanExampleCompilesAndDecides(t *testing.T) {
	c := mustCompile(t, `{
		"allow_private_access": true,
		"groups": {"group:oncall": ["user:cy@example.test"]},
		"tags": {"tag:prod": ["machine:db1"], "tag:ci": ["machine:runner"]},
		"rules": [
			{"src": ["role:member"], "dst": ["tag:ci"], "allow": ["terminal", "exec", "files"]},
			{"src": ["group:oncall"], "dst": ["tag:prod"], "allow": ["*"]},
			{"src": ["group:oncall"], "dst": ["tag:prod"], "allow": ["private_access"], "ports": [5432]},
			{"src": ["*"], "dst": ["tag:prod"], "deny": ["managed_ssh"], "note": "no SSH on prod"},
			{"src": ["group:oncall"], "dst": ["env:team"], "allow": ["env_read"]},
		],
		"tests": [
			{"src": "user:cy@example.test", "dst": "machine:db1", "allow": ["exec", "private_access:5432", "private_access:2000"], "deny": ["managed_ssh", "private_access:22"]},
			{"src": "user:bo@example.test", "dst": "machine:runner", "allow": ["exec"], "deny": ["managed_ssh"]},
			{"src": "user:bo@example.test", "dst": "machine:db1", "deny": ["exec"]},
			{"src": "user:cy@example.test", "dst": "env:team", "allow": ["env_read"], "deny": ["env_write"]},
		],
	}`, team())
	d := c.Decide("cy", machine("m-db1"), ActionManagedSSH, 0)
	if d.Allowed || d.Reason != "denied_by_rule" || !slices.Equal(d.Rules, []string{"/rules/3"}) {
		t.Fatalf("deny must win and name its rule: %+v", d)
	}
	d = c.Decide("cy", machine("m-db1"), ActionExec, 0)
	if !d.Allowed || !slices.Equal(d.Rules, []string{"/rules/1"}) {
		t.Fatalf("got %+v", d)
	}
	if d := c.Decide("cy", machine("m-db1"), ActionPrivateAccess, 0); !d.Allowed || !slices.Equal(d.Rules, []string{"/rules/1", "/rules/2"}) {
		t.Fatalf("got %+v", d)
	}
}

func TestDefaultDenyAndNoImpliedInspector(t *testing.T) {
	c := mustCompile(t, `{"rules": [{"src": ["*"], "dst": ["machine:web1"], "allow": ["preview_manage"]}]}`, team())
	pv := Target{Kind: "preview", ID: "pv-1"}
	if d := c.Decide("bo", pv, ActionInspect, 0); d.Allowed || d.Reason != "no_rule" {
		t.Fatalf("preview_manage must not imply inspect: %+v", d)
	}
	if d := c.Decide("bo", machine("m-db1"), ActionTerminal, 0); d.Allowed || d.Reason != "no_rule" {
		t.Fatalf("default deny: %+v", d)
	}
	if d := c.Decide("stranger", machine("m-db1"), ActionTerminal, 0); d.Allowed || d.Reason != "unknown_subject" {
		t.Fatalf("got %+v", d)
	}
}

func TestOwnerGuardrailBeatsDeny(t *testing.T) {
	c := mustCompile(t, `{"rules": [{"src": ["*"], "dst": ["machine:*"], "deny": ["*"]}]}`, personal())
	if d := c.Decide("ana", machine("m-laptop"), ActionTerminal, 0); !d.Allowed || d.Reason != "owner" {
		t.Fatalf("an owner cannot deny themselves out of their own machine: %+v", d)
	}
}

func TestTeamOwnershipGivesOnlyInspectorRights(t *testing.T) {
	c := mustCompile(t, `{"rules": [{"src": ["*"], "dst": ["machine:*"], "deny": ["inspect", "replay"]}]}`, team())
	pv := Target{Kind: "preview", ID: "pv-1"}
	if d := c.Decide("cy", pv, ActionReplay, 0); !d.Allowed || d.Reason != "owner" {
		t.Fatalf("the preview owner may always replay: %+v", d)
	}
	if d := c.Decide("cy", machine("m-web1"), ActionTerminal, 0); d.Allowed {
		t.Fatalf("in a team workspace a shared machine's owner needs a rule like everyone else (today's behaviour): %+v", d)
	}
	if d := c.Decide("bo", pv, ActionInspect, 0); d.Allowed || d.Reason != "denied_by_rule" {
		t.Fatalf("a deny on the machine covers its previews: %+v", d)
	}
}

func TestMachineInspectRuleCoversItsPreviews(t *testing.T) {
	c := mustCompile(t, `{"rules": [{"src": ["user:bo@example.test"], "dst": ["machine:web1"], "allow": ["inspect"]}]}`, team())
	pv := Target{Kind: "preview", ID: "pv-1"}
	if d := c.Decide("bo", pv, ActionInspect, 0); !d.Allowed || !slices.Equal(d.Rules, []string{"/rules/0"}) {
		t.Fatalf("got %+v", d)
	}
	if d := c.Decide("bo", pv, ActionReplay, 0); d.Allowed {
		t.Fatalf("inspect must not imply replay: %+v", d)
	}
}

func TestPrivateAccessPortsAndTeamSwitch(t *testing.T) {
	src := `{%s "rules": [
		{"src": ["*"], "dst": ["machine:db1"], "allow": ["private_access"]},
		{"src": ["*"], "dst": ["machine:db1"], "allow": ["private_access"], "ports": [22]},
		{"src": ["*"], "dst": ["machine:db1"], "deny": ["private_access"], "ports": ["3000-3999"]},
	]}`
	off := mustCompile(t, strings.Replace(src, "%s", "", 1), team())
	if d := off.Decide("cy", machine("m-db1"), ActionPrivateAccess, 8080); d.Allowed || d.Reason != "private_access_off" {
		t.Fatalf("allow_private_access defaults to false in a team: %+v", d)
	}
	if len(off.Enforced()) != 0 {
		t.Fatalf("enforced grants must drop private_access when the team switch is off: %v", off.Enforced())
	}
	on := mustCompile(t, strings.Replace(src, "%s", `"allow_private_access": true,`, 1), team())
	for port, want := range map[int]string{22: "rule", 80: "port_not_allowed", 3500: "denied_by_rule", 8080: "rule"} {
		if d := on.Decide("cy", machine("m-db1"), ActionPrivateAccess, port); d.Reason != want {
			t.Fatalf("port %d: got %+v, want %s", port, d, want)
		}
	}
	g := on.Enforced()[0]
	if got := portsLabel(g.Ports); got != "22,1024-2999,4000-65535" {
		t.Fatalf("ports %s", got)
	}
}

func TestScopedAPITokenNarrowsItsCreator(t *testing.T) {
	inv := personal()
	inv.Tokens = []Token{{ID: "tok-ci", Creator: "ana", Scope: []ScopeEntry{{Dst: []string{"tag:ci"}, Allow: []string{"exec"}}}}}
	c := mustCompile(t, `{"tags": {"tag:ci": ["machine:ci"]}}`, inv)
	if d := c.DecideToken("tok-ci", "ana", machine("m-ci"), ActionExec, 0); !d.Allowed {
		t.Fatalf("the scope allows exec on tag:ci: %+v", d)
	}
	for _, q := range []struct {
		target Target
		action string
	}{{machine("m-ci"), ActionTerminal}, {machine("m-laptop"), ActionExec}} {
		if d := c.DecideToken("tok-ci", "ana", q.target, q.action, 0); d.Allowed || d.Reason != "outside_token_scope" {
			t.Fatalf("%v %s: %+v", q.target, q.action, d)
		}
	}
	if d := c.DecideToken("tok-unscoped", "ana", machine("m-laptop"), ActionTerminal, 0); !d.Allowed {
		t.Fatalf("an unscoped token acts as its creator: %+v", d)
	}
	untagged := mustCompile(t, `{}`, inv)
	if d := untagged.DecideToken("tok-ci", "ana", machine("m-ci"), ActionExec, 0); d.Allowed {
		t.Fatalf("a scope whose tag is gone matches nothing: %+v", d)
	}
	if errs := untagged.ValidateScope(inv.Tokens[0].Scope); len(errs) != 1 || errs[0].Code != "unknown_tag" {
		t.Fatalf("creating a token with an unknown tag is refused: %v", errs)
	}
}

func TestScopedTokenNeverExceedsCreator(t *testing.T) {
	inv := team()
	inv.Tokens = []Token{{ID: "tok", Creator: "bo", Scope: []ScopeEntry{{Dst: []string{"machine:*"}, Allow: []string{"*"}}}}}
	c := mustCompile(t, `{"rules": [{"src": ["role:admin"], "dst": ["machine:runner"], "allow": ["exec"]}]}`, inv)
	if d := c.DecideToken("tok", "bo", machine("m-db1"), ActionExec, 0); d.Allowed {
		t.Fatalf("got %+v", d)
	}
	if d := c.DecideToken("tok", "bo", machine("m-runner"), ActionExec, 0); !d.Allowed {
		t.Fatalf("got %+v", d)
	}
}

func TestFailingTestBlocksAndNamesTheEntry(t *testing.T) {
	_, _, errs := Validate([]byte(`{
		"rules": [{"src": ["user:bo@example.test"], "dst": ["machine:db1"], "allow": ["exec"]}],
		"tests": [
			{"src": "user:bo@example.test", "dst": "machine:db1", "allow": ["exec", "terminal"], "deny": ["files", "exec"]},
			{"src": "user:zed@example.test", "dst": "machine:nope", "allow": ["warp"]},
		]
	}`), team(), Limits{})
	want := []string{"/tests/0/allow/1 test_failed", "/tests/0/deny/1 test_failed", "/tests/1/dst unknown_target", "/tests/1/src unknown_subject"}
	if got := codes(errs); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(errs[1].Message, "allowed by /rules/0") {
		t.Fatalf("a failed test explains the decision: %s", errs[1].Message)
	}
}

func TestChangesListsGainedAndLost(t *testing.T) {
	before := mustCompile(t, `{"rules": [{"src": ["*"], "dst": ["machine:db1"], "allow": ["exec", "terminal"]}]}`, team())
	after := mustCompile(t, `{"rules": [
		{"src": ["*"], "dst": ["machine:db1"], "allow": ["exec", "terminal"]},
		{"src": ["user:cy@example.test"], "dst": ["machine:db1"], "deny": ["terminal"]},
		{"src": ["user:bo@example.test"], "dst": ["machine:runner"], "allow": ["files"]},
	]}`, team())
	var got []string
	for _, ch := range Changes(before, after) {
		got = append(got, ch.Change+" "+ch.Subject+" "+ch.Target+" "+ch.Action)
	}
	want := []string{"gained user:bo@example.test machine:runner files", "lost user:cy@example.test machine:db1 terminal"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	if len(Changes(after, after)) != 0 {
		t.Fatal("no change, no entries")
	}
}

func TestWhoCanAndWhatCan(t *testing.T) {
	c := mustCompile(t, `{"rules": [{"src": ["role:admin"], "dst": ["machine:db1"], "allow": ["exec"]}]}`, team())
	var who []string
	for _, a := range c.WhoCan(machine("m-db1"), ActionExec) {
		who = append(who, a.Subject)
	}
	if !reflect.DeepEqual(who, []string{"user:ana@example.test", "user:bo@example.test"}) {
		t.Fatalf("got %q", who)
	}
	what := c.WhatCan("cy")
	if len(what) != 2 || what[0].Reason != "owner" || what[0].Action != ActionInspect {
		t.Fatalf("cy owns web1, so inspect/replay on it only: %+v", what)
	}
}

func TestCanonicalDropsComments(t *testing.T) {
	out, err := Canonical([]byte("{\n // c\n \"rules\": [],\n}"))
	if err != nil || string(out) != `{"rules":[]}` {
		t.Fatalf("%s %v", out, err)
	}
}

func TestSameAliasNeedsTheOwnerQualifiedName(t *testing.T) {
	inv := team()
	inv.Machines = append(inv.Machines, Machine{ID: "m-web1b", Alias: "web1", PersonalOwner: "bo"})
	d, _ := Parse([]byte(`{"rules": [{"src": ["*"], "dst": ["machine:web1"], "allow": ["exec"]}]}`))
	if _, errs := Compile(d, inv, Limits{}); len(errs) != 1 || errs[0].Code != "ambiguous_machine" {
		t.Fatalf("got %v", errs)
	}
	c := mustCompile(t, `{"rules": [{"src": ["*"], "dst": ["machine:Bo@example.test/web1"], "allow": ["exec"]}]}`, inv)
	if d := c.Decide("ana", machine("m-web1b"), ActionExec, 0); !d.Allowed {
		t.Fatalf("got %+v", d)
	}
	if got := inv.MachineName("m-web1"); got != "cy@example.test/web1" {
		t.Fatalf("name %q", got)
	}
	if got := inv.MachineName("m-db1"); got != "db1" {
		t.Fatalf("name %q", got)
	}
	inv.Machines = append(inv.Machines, Machine{ID: "m-db1b", Alias: "db1", PersonalOwner: "bo"})
	c = mustCompile(t, `{"rules": [{"src": ["*"], "dst": ["machine:db1"], "allow": ["exec"]}]}`, inv)
	if d := c.Decide("ana", machine("m-db1"), ActionExec, 0); !d.Allowed || c.Decide("ana", machine("m-db1b"), ActionExec, 0).Allowed {
		t.Fatal("the team-owned machine keeps the plain alias")
	}
	if inv.MachineName("m-db1") != "db1" || inv.MachineName("m-db1b") != "bo@example.test/db1" {
		t.Fatal(inv.MachineName("m-db1"), inv.MachineName("m-db1b"))
	}
}

func TestPortRangeJSONRoundTrip(t *testing.T) {
	var got []PortRange
	if err := json.Unmarshal([]byte(`[22, "8000-8100"]`), &got); err != nil || len(got) != 2 || got[1].To != 8100 {
		t.Fatal(got, err)
	}
	out, _ := json.Marshal(got)
	if string(out) != `[22,"8000-8100"]` {
		t.Fatal(string(out))
	}
	if err := json.Unmarshal([]byte(`[0]`), &got); err == nil {
		t.Fatal("port 0 accepted")
	}
}

func TestExactRolesMatchOnlyThatRole(t *testing.T) {
	inv := team()
	inv.Members = append(inv.Members, Member{AccountID: "di", Email: "di@example.test", Role: "billing"})
	c := mustCompile(t, `{"rules": [{"src": ["role:billing"], "dst": ["machine:db1"], "allow": ["exec"]}]}`, inv)
	for account, want := range map[string]bool{"di": true, "ana": false, "bo": false, "cy": false} {
		if got := c.Decide(account, machine("m-db1"), ActionExec, 0).Allowed; got != want {
			t.Errorf("%s: %v", account, got)
		}
	}
	for _, r := range []struct {
		name, role string
		want       bool
	}{{"member", "billing", true}, {"admin", "owner", true}, {"admin", "billing", false}, {"owner", "admin", false}, {"auditor", "auditor", true}} {
		if RoleMatches(r.name, r.role) != r.want {
			t.Errorf("RoleMatches(%s, %s)", r.name, r.role)
		}
	}
}

func TestTeamOwnerKeepsTeamENVAgainstAnyDeny(t *testing.T) {
	c := mustCompile(t, `{"rules": [
		{"src": ["*"], "dst": ["env:team"], "deny": ["env_read", "env_write"]},
		{"src": ["role:admin"], "dst": ["env:team"], "allow": ["env_read", "env_write"]},
	]}`, team())
	env := Target{Kind: "env", ID: "acme"}
	for _, action := range []string{ActionENVRead, ActionENVWrite} {
		if d := c.Decide("ana", env, action, 0); !d.Allowed || d.Reason != "owner" {
			t.Fatalf("the team owner keeps the team ENV (%s): %+v", action, d)
		}
		if d := c.Decide("bo", env, action, 0); d.Allowed || d.Reason != "denied_by_rule" {
			t.Fatalf("an admin follows the rules (%s): %+v", action, d)
		}
	}
}

func TestChangesNeverListTheOwnerGuardrail(t *testing.T) {
	before := mustCompile(t, `{"rules": [{"src": ["role:admin"], "dst": ["env:team"], "allow": ["env_read", "env_write"]}]}`, team())
	after := mustCompile(t, `{"rules": []}`, team())
	var got []string
	for _, c := range Changes(before, after) {
		got = append(got, c.Change+" "+c.Subject+" "+c.Action)
	}
	want := []string{"lost user:bo@example.test env_read", "lost user:bo@example.test env_write"}
	if !slices.Equal(got, want) {
		t.Fatalf("the owner keeps the team ENV, so only the admin loses it: %q", got)
	}
}
