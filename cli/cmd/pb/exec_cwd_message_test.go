package main

import (
	"strings"
	"testing"
)

func TestExecWorkingDirectoryRefusalMessageNamesTheRule(t *testing.T) {
	got := localAPIFailureMessage("exec_cwd_unavailable")
	for _, want := range []string{"--cwd", "workspace", "Nothing was run"} {
		if !strings.Contains(got, want) {
			t.Fatalf("message %q lacks %q", got, want)
		}
	}
}
