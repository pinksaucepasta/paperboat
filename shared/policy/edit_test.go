package policy

import (
	"reflect"
	"strings"
	"testing"
)

func TestSetSimpleRuleEditsOnlyItsOwnRules(t *testing.T) {
	src := []byte(`{
	// team policy
	"rules": [
		{"src": ["role:member"], "dst": ["machine:db1"], "allow": ["exec"]},
		{"src": ["group:ops"], "dst": ["machine:db1"], "allow": ["exec"]},
		{"src": ["role:member"], "dst": ["machine:db1"], "allow": ["files"], "note": "kept: has a note"},
	],
}`)
	out, err := SetSimpleRule(src, "role:member", "machine:db1", []string{"terminal", "exec"})
	if err != nil {
		t.Fatal(err)
	}
	d, errs := Parse(out)
	if len(errs) > 0 {
		t.Fatal(errs, string(out))
	}
	if !strings.Contains(string(out), "// team policy") || len(d.Rules) != 3 {
		t.Fatalf("%s", out)
	}
	if got := SimpleRules(d); !reflect.DeepEqual(got, []SimpleRule{{Subject: "role:member", Target: "machine:db1", Allow: []string{"exec", "terminal"}}}) {
		t.Fatalf("%+v", got)
	}
	out, _ = SetSimpleRule(out, "role:member", "machine:db1", nil)
	d, _ = Parse(out)
	if len(SimpleRules(d)) != 0 || len(d.Rules) != 2 {
		t.Fatalf("%s", out)
	}
	empty, _ := SetSimpleRule([]byte(`{}`), "user:bo@example.test", "machine:runner", []string{"exec"})
	d, _ = Parse(empty)
	if len(d.Rules) != 1 || d.Rules[0].Src[0] != "user:bo@example.test" {
		t.Fatalf("%s", empty)
	}
}

func TestSetAllowPrivateAccess(t *testing.T) {
	out, _ := SetAllowPrivateAccess([]byte(`{"rules": []}`), true)
	d, _ := Parse(out)
	if d.AllowPrivateAccess == nil || !*d.AllowPrivateAccess {
		t.Fatalf("%s", out)
	}
	out, _ = SetAllowPrivateAccess(out, false)
	d, _ = Parse(out)
	if *d.AllowPrivateAccess {
		t.Fatalf("%s", out)
	}
}

func TestSetSimpleRuleOnTheTeamENV(t *testing.T) {
	src := []byte(`{"rules": [{"src": ["role:admin"], "dst": ["env:team"], "allow": ["env_read", "env_write"]}]}`)
	out, err := SetSimpleRule(src, "user:cy@example.test", "env:team", []string{ActionENVRead})
	if err != nil {
		t.Fatal(err)
	}
	d, errs := Parse(out)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	want := []SimpleRule{{Subject: "user:cy@example.test", Target: "env:team", Allow: []string{ActionENVRead}}}
	if got := SimpleRules(d); !reflect.DeepEqual(got, want) || len(d.Rules) != 2 {
		t.Fatalf("role:admin is not the simple view's; got %+v in %s", got, out)
	}
	out, _ = SetSimpleRule(out, "user:cy@example.test", "env:team", nil)
	if d, _ = Parse(out); len(d.Rules) != 1 || len(SimpleRules(d)) != 0 {
		t.Fatalf("%s", out)
	}
	if _, ok := simple(Rule{Src: []string{"role:member"}, Dst: []string{"env:team"}, Allow: []string{ActionExec}}); ok {
		t.Fatal("a machine action on env:team is not a simple rule")
	}
}

func TestSetSimpleRuleOnAPreview(t *testing.T) {
	out, err := SetSimpleRule([]byte(`{}`), "user:cy@example.test", "preview:p1", []string{ActionReplay, ActionInspect})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := Parse(out)
	want := []SimpleRule{{Subject: "user:cy@example.test", Target: "preview:p1", Allow: []string{ActionInspect, ActionReplay}}}
	if got := SimpleRules(d); !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	if _, ok := simple(Rule{Src: []string{"role:member"}, Dst: []string{"tunnel:api"}, Allow: []string{ActionExec}}); ok {
		t.Fatal("a machine action on a tunnel is not a simple rule")
	}
}

func TestSetMachineTagsEditsOnlyThatMachine(t *testing.T) {
	src := []byte(`{
	// tags
	"tags": {"tag:prod": ["machine:db1", "machine:web1"], "tag:old": ["machine:db1"], "tag:ci": ["machine:db1"]},
	"rules": [{"src": ["*"], "dst": ["tag:ci"], "allow": ["exec"]}],
}`)
	out, err := SetMachineTags(src, "machine:db1", []string{"tag:prod", "tag:new"})
	if err != nil {
		t.Fatal(err)
	}
	d, errs := Parse(out)
	if len(errs) > 0 {
		t.Fatal(errs, string(out))
	}
	want := map[string][]string{"tag:prod": {"machine:db1", "machine:web1"}, "tag:new": {"machine:db1"}, "tag:ci": {}}
	for name, machines := range want {
		got, ok := d.Tags[name]
		if !ok || len(got) != len(machines) {
			t.Fatalf("%s = %v (present %v), want %v\n%s", name, got, ok, machines, out)
		}
	}
	if _, ok := d.Tags["tag:old"]; ok {
		t.Fatalf("an emptied tag no rule uses is removed:\n%s", out)
	}
	if !strings.Contains(string(out), "// tags") {
		t.Fatalf("comments are kept:\n%s", out)
	}
	if got := MachineTags(d, "machine:db1"); !reflect.DeepEqual(got, []string{"tag:new", "tag:prod"}) {
		t.Fatalf("MachineTags = %v", got)
	}
	empty, _ := SetMachineTags([]byte(`{}`), "machine:runner", []string{"tag:ci"})
	if d, _ = Parse(empty); len(d.Tags["tag:ci"]) != 1 {
		t.Fatalf("%s", empty)
	}
}
