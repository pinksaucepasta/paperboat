//go:build linux

package machineguard

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// With real certutil: a write killed midway leaves a hot journal; the heal rolls it back and -L lists again.
func TestNSSHealsARealHotJournal(t *testing.T) {
	if _, err := exec.LookPath("certutil"); err != nil {
		t.Skip("certutil (NSS tools) is not installed")
	}
	_, _, rootPEM := renewalFixture(t, 365)
	root, err := parseLocalTrustRoot(rootPEM)
	if err != nil {
		t.Fatal(err)
	}
	database, pemPath := t.TempDir(), filepath.Join(t.TempDir(), "root.pem")
	if err = os.WriteFile(pemPath, rootPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = runNSSCommand(t.Context(), "-N", "-d", "sql:"+database, "--empty-password"); err != nil {
		t.Fatal(err)
	}
	if _, err = runNSSCommand(t.Context(), "-A", "-d", "sql:"+database, "-t", "C,,", "-n", localTrustNickname(root), "-i", pemPath); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; !nssHotJournal(database); attempt++ {
		if attempt == 300 {
			t.Skip("could not interrupt a certutil write at the right moment on this host")
		}
		command := exec.Command("certutil", "-A", "-d", "sql:"+database, "-t", "C,,", "-n", fmt.Sprintf("interrupted %d", attempt), "-i", pemPath)
		if err = command.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Duration(attempt%40) * time.Millisecond / 2)
		_ = command.Process.Kill()
		_ = command.Wait()
	}
	present, err := nssLocalRoot(t.Context(), database, root, runNSSCommand, true)
	if err != nil || !present {
		t.Fatalf("after the heal: present=%v err=%v", present, err)
	}
	if _, err = runNSSCommand(t.Context(), "-L", "-d", "sql:"+database); err != nil {
		t.Fatalf("store still unreadable: %v", err)
	}
}
