package policy

import (
	"strings"
	"testing"
)

func TestRewriteRenamesAndRemovesKeepingComments(t *testing.T) {
	src := `{
	// prod machines
	"tags": {"tag:prod": ["machine:db1", "machine:web1"]},
	"groups": {"group:ops": ["user:bo@example.test", "user:cy@example.test"]},
	"rules": [
		{"src": ["group:ops"], "dst": ["machine:db1"], "allow": ["exec"]},
		{"src": ["user:cy@example.test"], "dst": ["machine:runner"], "allow": ["exec"]}, // cy only
		{"src": ["*"], "dst": ["machine:web1"], "allow": ["files"]},
	],
	"tests": [
		{"src": "user:cy@example.test", "dst": "machine:runner", "allow": ["exec"]},
		{"src": "user:bo@example.test", "dst": "machine:db1", "allow": ["exec"]},
	],
}`
	out, changed, err := Rewrite([]byte(src), map[string]string{"machine:db1": "machine:db9"},
		map[string]bool{"user:cy@example.test": true, "machine:web1": true})
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	got := string(out)
	for _, want := range []string{"// prod machines", `"tag:prod": ["machine:db9"]`, `"group:ops": ["user:bo@example.test"]`, `"dst": ["machine:db9"]`, `"dst": "machine:db9"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in\n%s", want, got)
		}
	}
	for _, gone := range []string{"cy@example.test", "machine:web1", "machine:runner"} {
		if strings.Contains(got, gone) {
			t.Errorf("%s should be gone (its rule/test lost src or dst):\n%s", gone, got)
		}
	}
	inv := team()
	inv.Machines[0].Alias = "db9"
	if _, errs := Parse(out); len(errs) > 0 {
		t.Fatalf("rewritten file must parse: %v", errs)
	}
	if _, changed, _ := Rewrite(out, map[string]string{"machine:zzz": "machine:y"}, nil); changed {
		t.Fatal("nothing matched, nothing changed")
	}
}

func TestStoreKeepsSubjectsAndTargetsSymbolic(t *testing.T) {
	c := mustCompile(t, `{"allow_private_access": true,
		"groups": {"group:ops": ["user:bo@example.test"]},
		"tags": {"tag:prod": ["machine:db1"]},
		"rules": [
			{"src": ["role:member", "group:ops"], "dst": ["tag:prod", "machine:*"], "allow": ["exec"]},
			{"src": ["user:cy@example.test"], "dst": ["machine:db1"], "deny": ["private_access"], "ports": [22]},
		]}`, team())
	s := c.Store()
	if len(s.Rows) != 5 || !s.AllowPrivateAccess || s.Tags["tag:prod"][0] != "m-db1" || s.Groups["group:ops"][0] != "bo" {
		t.Fatalf("%+v", s)
	}
	last := s.Rows[4]
	if !last.Deny || last.SubjectKind != "account" || last.SubjectID != "cy" || last.TargetKind != "machine" || last.TargetID != "m-db1" || portsLabel(last.Ports) != "22" {
		t.Fatalf("%+v", last)
	}
	first := s.Rows[0]
	if first.SubjectKind != "role" || first.SubjectID != "member" || first.TargetKind != "tag" || first.TargetID != "tag:prod" || portsLabel(first.Ports) != "1-65535" {
		t.Fatalf("%+v", first)
	}
}

func TestInternalizeAndExternalizeRoundTrip(t *testing.T) {
	inv := team()
	src := []byte(`{
	// ids survive renames
	"tags": {"tag:prod": ["machine:db1"]},
	"rules": [{"src": ["user:Cy@example.test", "role:member"], "dst": ["machine:web1", "machine:*"], "allow": ["exec"], "note": "user:cy@example.test"}],
	"tests": [{"src": "user:cy@example.test", "dst": "machine:db1", "allow": ["exec"]}],
}`)
	in, err := Internalize(src, inv)
	if err != nil {
		t.Fatal(err)
	}
	s := string(in)
	for _, want := range []string{`"machine:#m-db1"`, `"user:#cy"`, `"machine:#m-web1"`, `"machine:*"`, `"note": "user:cy@example.test"`, "// ids survive renames"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s:\n%s", want, s)
		}
	}
	inv.Machines[0].Alias = "db9"
	out, err := Externalize(in, inv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"machine:db9"`) || !strings.Contains(string(out), `"user:cy@example.test"`) {
		t.Fatalf("rename shows the new name:\n%s", out)
	}
	inv.Members = inv.Members[:2]
	out, _ = Externalize(in, inv)
	if strings.Contains(string(out), `["user:cy`) || strings.Contains(string(out), `"src": "user`) || !strings.Contains(string(out), `"src": ["role:member"]`) {
		t.Fatalf("a departed member's references and tests are dropped:\n%s", out)
	}
	if _, errs := Parse(out); len(errs) > 0 {
		t.Fatal(errs)
	}
}
