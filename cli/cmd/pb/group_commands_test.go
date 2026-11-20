package main

import (
	"bytes"
	"strings"
	"testing"
)

// command rows C144/C146/C159/C005: typos in command groups exited 0.
func TestCommandGroupsRejectUnknownWordsAndListJSON(t *testing.T) {
	for _, group := range [][]string{{"env", "grants"}, {"env", "host"}, {"env", "vault"}, {"daemon", "machine-guard"}} {
		var out bytes.Buffer
		if code := run(t.Context(), append(append([]string{}, group...), "bogus"), &out, &out); code == 0 || !strings.Contains(out.String(), "arguments are invalid") && !strings.Contains(out.String(), "is not a") {
			t.Fatalf("%v bogus: exit=%d output=%s", group, code, out.String())
		}
	}
	var out bytes.Buffer
	if code := run(t.Context(), []string{"env", "grants", "--json"}, &out, &out); code != 0 || !strings.Contains(out.String(), `"commands"`) {
		t.Fatalf("env grants --json: exit=%d output=%s", code, out.String())
	}
	out.Reset()
	if code := run(t.Context(), []string{"env", "grants"}, &out, &out); code != 0 || !strings.Contains(out.String(), "Usage") {
		t.Fatalf("env grants without arguments should still show help: exit=%d output=%s", code, out.String())
	}
}

func TestUpdateBundleExplainsMissingOutput(t *testing.T) {
	var out bytes.Buffer
	if code := run(t.Context(), []string{"update", "bundle"}, &out, &out); code == 0 || !strings.Contains(out.String(), "--output is required") {
		t.Fatalf("exit=%d output=%s", code, out.String())
	}
}
