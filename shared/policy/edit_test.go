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
	if got := SimpleRules(d); !reflect.DeepEqual(got, []SimpleRule{{Subject: "role:member", Machine: "machine:db1", Allow: []string{"exec", "terminal"}}}) {
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
