package main

import (
	"regexp"
	"strings"
	"testing"
)

var internalName = regexp.MustCompile(`(?i)\b(hostd|paperboatd|lane [a-z])\b`)

// A command's help text has one owner: no path in two tables, none that also keeps an old
// detail entry, and no internal process name in text users read.
func TestHelpTextOwnership(t *testing.T) {
	root := newRootCommand()
	seen := map[string]bool{}
	for _, group := range helpTextGroups {
		for path, text := range group {
			if seen[path] {
				t.Errorf("%s has help text in two tables", path)
			}
			seen[path] = true
			if text.Long != "" {
				if _, old := commandHelpDetails[path]; old {
					t.Errorf("%s has help text and an old commandHelpDetails entry", path)
				}
			}
			command, _, err := root.Find(strings.Fields(path)[1:])
			if err != nil || command.CommandPath() != path {
				t.Errorf("%s is not a command", path)
				continue
			}
			if internalName.MatchString(text.Long + text.Example) {
				t.Errorf("%s help text names an internal process or lane", path)
			}
		}
	}
}

// Every example in the help tables must be accepted by the command it documents: known flags, required
// flags present, and an argument count the command takes.
func TestHelpTextExamplesParse(t *testing.T) {
	root := newRootCommand()
	for _, group := range helpTextGroups {
		for path, text := range group {
			for _, line := range strings.Split(text.Example, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				line, _, _ = strings.Cut(line, " > ") // a shell redirect is not an argument
				fields := strings.Fields(strings.NewReplacer("'", "").Replace(line))
				command, rest, err := root.Find(fields[1:])
				if err != nil || command == root {
					t.Errorf("%s: example %q does not reach a command (%v)", path, line, err)
					continue
				}
				if command.DisableFlagParsing {
					continue
				}
				if err := command.ParseFlags(rest); err != nil {
					t.Errorf("%s: example %q: %v", path, line, err)
					continue
				}
				if err := command.ValidateRequiredFlags(); err != nil {
					t.Errorf("%s: example %q: %v", path, line, err)
				}
				if command.Args != nil {
					if err := command.Args(command, command.Flags().Args()); err != nil {
						t.Errorf("%s: example %q: %v", path, line, err)
					}
				}
			}
		}
	}
}
