//go:build linux

package machineguard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A cancelled caller deadline must not kill a certutil write midway (it left the user's Chrome store unreadable).
func TestNSSWriteSurvivesACancelledCaller(t *testing.T) {
	bin, marker := t.TempDir(), filepath.Join(t.TempDir(), "finished")
	script := "#!/bin/sh\nsleep 1\necho done > " + marker + "\n"
	if err := os.WriteFile(filepath.Join(bin, "certutil"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runNSSCommand(ctx, "-A", "-d", "sql:/nowhere"); err != nil {
		t.Fatalf("write was cancelled: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("certutil was killed before it finished")
	}
	if _, err := runNSSCommand(ctx, "-L", "-d", "sql:/nowhere"); err == nil {
		t.Fatal("a read must still follow the caller's deadline")
	}
}

func TestNSSHotJournalIsRolledBackBeforeTheReadOnlyListing(t *testing.T) {
	_, _, rootPEM := renewalFixture(t, 365)
	root, err := parseLocalTrustRoot(rootPEM)
	if err != nil {
		t.Fatal(err)
	}
	for name, journal := range map[string]bool{"hot journal": true, "no journal": false} {
		t.Run(name, func(t *testing.T) {
			database := t.TempDir()
			if journal {
				if err := os.WriteFile(filepath.Join(database, "cert9.db-journal"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var calls []string
			run := func(_ context.Context, args ...string) ([]byte, error) {
				calls = append(calls, strings.Join(args, " "))
				return nil, nil
			}
			if _, err := nssLocalRoot(context.Background(), database, root, run, true); err != nil {
				t.Fatal(err)
			}
			recovery := "-D -d sql:" + database + " -n " + nssJournalRecoveryNickname
			if journal && (len(calls) < 2 || calls[0] != recovery || !strings.HasPrefix(calls[1], "-L ")) {
				t.Fatalf("calls = %q, want the write-mode open first, then -L", calls)
			}
			if !journal && (len(calls) == 0 || !strings.HasPrefix(calls[0], "-L ")) {
				t.Fatalf("calls = %q, want only the read-only listing", calls)
			}
			if _, err := os.Stat(filepath.Join(database, "cert9.db-journal")); journal && err != nil {
				t.Fatal("the journal must never be deleted by Paperboat")
			}
		})
	}
}
