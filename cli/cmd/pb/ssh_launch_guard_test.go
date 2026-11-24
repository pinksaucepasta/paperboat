package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	doctorpkg "github.com/pinksaucepasta/paperboat/cli/internal/doctor"
)

func TestRequireManagedSSHConfiguration(t *testing.T) {
	const include = "Include ~/.ssh/paperboat_config # paperboat-managed-ssh-include-v1"
	const fragment = "Match host *.example.test\n  ProxyCommand pb __ssh-proxy\n"
	for name, test := range map[string]struct {
		main, fragment string
		ok             bool
	}{
		"installed":        {include + "\n", fragment, true},
		"CRLF":             {include + "\r\nHost x\r\n", fragment, true},
		"include missing":  {"Host x\n", fragment, false},
		"include disabled": {"# " + include + "\n", fragment, false},
		"include nested":   {"Host x\n  " + include + "\n", fragment, false},
		"other file":       {"Include ~/.ssh/paperboat_config.old\n", fragment, false},
		"fragment missing": {include + "\n", "", false},
		"both missing":     {"", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			if err := os.Mkdir(filepath.Join(home, ".ssh"), 0o700); err != nil {
				t.Fatal(err)
			}
			for file, text := range map[string]string{"config": test.main, "paperboat_config": test.fragment} {
				if text != "" {
					if err := os.WriteFile(filepath.Join(home, ".ssh", file), []byte(text), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			err := requireManagedSSHConfiguration()
			var refused commandPreparationFailure
			if test.ok != (err == nil) || !test.ok && (!errors.As(err, &refused) || refused.step != prepareSSHConfiguration) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestDoctorNamesAnIncludeThatDoesNotApply(t *testing.T) {
	const include = "Include ~/.ssh/paperboat_config # paperboat-managed-ssh-include-v1\n"
	for name, test := range map[string]struct{ main, summary string }{
		"in place":  {include + "Host x\n", ""},
		"nested":    {"Host=x\n  " + include, "inside a Host or Match block"},
		"commented": {"# " + include + "Host x\n", "commented out"},
		"missing":   {"Host x\n", "missing"},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			if err := os.Mkdir(filepath.Join(home, ".ssh"), 0o700); err != nil {
				t.Fatal(err)
			}
			for file, text := range map[string]string{"config": test.main, "paperboat_config": "x"} {
				if err := os.WriteFile(filepath.Join(home, ".ssh", file), []byte(text), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			check := doctorSSHIncludeCheck(t.Context())
			if test.summary == "" && check.Status != doctorpkg.StatusPass ||
				test.summary != "" && (check.Status != doctorpkg.StatusWarning || !strings.Contains(check.Summary, test.summary) || check.Recovery == "") {
				t.Fatalf("check=%+v", check)
			}
		})
	}
}
