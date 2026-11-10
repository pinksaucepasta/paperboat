package processlaunch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var execCall = regexp.MustCompile(`exec\.Command(Context)?\(`)

// Every file that can build on Windows and starts a process must hide it with ConfigureBackground, or say with a
// "user-console:" comment why the user's own terminal is meant to show it.
func TestWindowsProcessStartsAreHiddenOrUserConsole(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if entry.IsDir() {
			if rel == "tools" || entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		for _, other := range []string{"_linux.go", "_darwin.go", "_unix.go", "_other.go"} {
			if strings.HasSuffix(name, other) {
				return nil
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		if strings.Contains(text, "//go:build !windows") || !execCall.MatchString(text) {
			return nil
		}
		if !strings.Contains(text, "ConfigureBackground") && !strings.Contains(text, "user-console:") {
			t.Errorf("%s starts a process without processlaunch.ConfigureBackground or a user-console: comment", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
