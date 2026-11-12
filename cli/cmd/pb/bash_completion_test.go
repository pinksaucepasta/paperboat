package main

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestBashCompletionNamesMissingPrerequisite(t *testing.T) {
	root := newRootCommand()
	var script bytes.Buffer
	root.SetOut(&script)
	root.SetArgs([]string{"completion", "bash"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	cmd := exec.Command(bash, "--noprofile", "--norc", "-c", "source /dev/stdin")
	cmd.Stdin = &script
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "brew install bash bash-completion@2") {
		t.Fatalf("missing actionable prerequisite error: %v %s", err, output)
	}
	root = newRootCommand()
	var help bytes.Buffer
	root.SetOut(&help)
	root.SetArgs([]string{"completion", "bash", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(help.String(), "brew install bash bash-completion@2") {
		t.Fatalf("help lacks prerequisite: %s", &help)
	}
}
