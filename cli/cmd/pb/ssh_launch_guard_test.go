package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
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
