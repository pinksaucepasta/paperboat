package containers

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSentryEnvironmentPassesTheDSNFilePathNeverTheDSN(t *testing.T) {
	dir := t.TempDir()
	if args, err := sentryEnvironment(dir); err != nil || args != nil {
		t.Fatalf("no file: %v %v", args, err)
	}
	path := filepath.Join(dir, SentryDSNFile)
	const dsn = "https://publickey@example.invalid/1"
	if err := os.WriteFile(path, []byte(dsn), 0o600); err != nil {
		t.Fatal(err)
	}
	args, err := sentryEnvironment(dir)
	want := []string{"--env", "PAPERBOAT_SENTRY_ENABLED=true", "--env", "PAPERBOAT_SENTRY_DSN_FILE=" + path}
	if err != nil || !slices.Equal(args, want) {
		t.Fatalf("args=%v err=%v", args, err)
	}
	if strings.Contains(strings.Join(args, " "), "publickey") {
		t.Fatal("the DSN reached the docker command line")
	}
}

func TestSentryEnvironmentRefusesUnsafeFiles(t *testing.T) {
	for name, prepare := range map[string]func(path string) error{
		"group readable": func(path string) error { return os.WriteFile(path, []byte("x"), 0o640) },
		"oversized":      func(path string) error { return os.WriteFile(path, make([]byte, maxSentryDSNBytes+1), 0o600) },
		"symlink": func(path string) error {
			target := filepath.Join(filepath.Dir(path), "target")
			if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
				return err
			}
			return os.Symlink(target, path)
		},
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, SentryDSNFile)
		if err := prepare(path); err != nil {
			t.Fatal(err)
		}
		if _, err := sentryEnvironment(dir); err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
}
