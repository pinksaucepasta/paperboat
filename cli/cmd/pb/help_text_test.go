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
