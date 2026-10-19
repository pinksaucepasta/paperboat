package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

var (
	docsCommandSpan = regexp.MustCompile("`(pb [^`\\n]+)`")
	docsShellLine   = regexp.MustCompile(`(?m)^\s*(?:\$ )?(pb [^\n#]+)`)
	docsWord        = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	docsFlag        = regexp.MustCompile(`^--[a-z][a-z0-9-]*`)
)

// The handwritten guides name commands and flags users type. A command or flag
// that no longer exists in the command tree fails here instead of misleading users.
func TestDocumentedCommandsAndFlagsExist(t *testing.T) {
	root := newRootCommand()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	var pages []string
	for _, pattern := range []string{"../../README.md", "../../docs/*.md"} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, matches...)
	}
	if len(pages) < 5 {
		t.Fatalf("found %d documentation pages; the documentation moved", len(pages))
	}
	checked := 0
	for _, page := range pages {
		data, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		var invocations []string
		for _, match := range docsCommandSpan.FindAllStringSubmatch(string(data), -1) {
			invocations = append(invocations, match[1])
		}
		for _, match := range docsShellLine.FindAllStringSubmatch(string(data), -1) {
			invocations = append(invocations, match[1])
		}
		for _, invocation := range invocations {
			checked++
			if problem := documentedInvocationProblem(root, invocation); problem != "" {
				t.Errorf("%s: `%s`: %s", filepath.Base(page), invocation, problem)
			}
		}
	}
	if checked < 20 {
		t.Fatalf("checked only %d documented invocations", checked)
	}
}

func documentedInvocationProblem(root *cobra.Command, invocation string) string {
	tokens := strings.Fields(invocation)[1:]
	commands := []*cobra.Command{root} // "set/unset/list" names several commands at once
	var flags []string
	argument := false // after the first non-command word, the rest are arguments
	for _, token := range tokens {
		switch {
		case docsFlag.MatchString(token):
			flags = append(flags, docsFlag.FindString(token))
		case argument || len(flags) > 0 || !docsWord.MatchString(strings.ReplaceAll(token, "/", "")):
			argument = true
		default:
			var next []*cobra.Command
			for _, name := range strings.Split(token, "/") {
				for _, command := range commands {
					if child := findChild(command, name); child != nil {
						next = append(next, child)
					} else if command != root && command.HasSubCommands() && !command.Runnable() {
						return "pb " + command.CommandPath()[len(root.Name())+1:] + " has no subcommand " + name
					}
				}
			}
			if len(next) == 0 {
				argument = true
			} else {
				commands = next
			}
		}
	}
	for _, flag := range flags {
		name := strings.TrimPrefix(flag, "--")
		if name == "help" {
			continue
		}
		for _, command := range commands {
			if command.Flags().Lookup(name) == nil && command.InheritedFlags().Lookup(name) == nil && root.PersistentFlags().Lookup(name) == nil {
				return command.CommandPath() + " has no flag " + flag
			}
		}
	}
	return ""
}

func findChild(command *cobra.Command, name string) *cobra.Command {
	for _, child := range command.Commands() {
		if child.Name() == name || child.HasAlias(name) {
			return child
		}
	}
	return nil
}

func TestDocumentedInvocationCheckCatchesStaleCommands(t *testing.T) {
	root := &cobra.Command{Use: "pb"}
	group := &cobra.Command{Use: "group"}
	leaf := &cobra.Command{Use: "leaf", Run: func(*cobra.Command, []string) {}}
	leaf.Flags().Bool("json", false, "")
	other := &cobra.Command{Use: "other", Run: func(*cobra.Command, []string) {}}
	group.AddCommand(leaf, other)
	root.AddCommand(group)
	root.PersistentFlags().String("server", "", "")
	for invocation, wantProblem := range map[string]bool{
		"pb group leaf --json":           false,
		"pb group leaf --help":           false,
		"pb group leaf extra --server x": false,
		"pb group leaf/other --json":     true, // other has no --json
		"pb group gone":                  true,
		"pb group leaf --gone":           true,
	} {
		if got := documentedInvocationProblem(root, invocation) != ""; got != wantProblem {
			t.Errorf("%q: problem reported = %v, want %v", invocation, got, wantProblem)
		}
	}
}
