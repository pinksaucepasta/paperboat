//go:build darwin || linux

package workerupdate

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
)

func TestFreshManagerRecoversPromotedDistinctExecutable(t *testing.T) {
	f := newFixture(t)
	f.fetcher.body = append(append([]byte(nil), f.fetcher.body...), []byte("candidate release")...)
	f.candidate = release(f.candidate.Version, f.fetcher.body)
	// The native package fixture's extraction returns these same distinct bytes.
	if err := os.WriteFile(f.paths.root+"/installed/pb", f.fetcher.body, 0700); err != nil {
		t.Fatal(err)
	}
	f.starter.activateError = errors.New("activation response lost")
	if _, err := f.manager.Activate(context.Background(), f.candidate); err == nil {
		t.Fatal("expected uncertain cutover")
	}
	restarted, err := New(f.manager.config)
	if err != nil {
		t.Fatalf("fresh recovery manager rejected authenticated promoted executable: %v", err)
	}
	f.starter.activateError = nil
	if err := restarted.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if restarted.ActiveVersion() != f.candidate.Version {
		t.Fatal("candidate not recovered")
	}
}

func TestFreshManagerRejectsUnboundPromotedExecutable(t *testing.T) {
	for _, scenario := range []string{"no-journal", "pre-cutover", "wrong-active", "wrong-digest", "symlink-journal"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			candidate := append(append([]byte(nil), f.fetcher.body...), 'x')
			f.candidate = release(f.candidate.Version, candidate)
			j := withRelease(f.manager.newJournal(), f.candidate, f.paths.current)
			j.Stage = updateflow.StageCutover
			j.WorkerID, j.WorkerEpoch = "candidate", 2
			if scenario == "pre-cutover" {
				j.Stage = updateflow.StageCandidateReady
			}
			if scenario == "wrong-active" {
				j.ActiveDigest = f.candidate.SHA256
			}
			if err := f.manager.write(j); err != nil {
				t.Fatal(err)
			}
			if scenario == "no-journal" {
				if err := os.Remove(f.paths.journal); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "symlink-journal" {
				if err := os.Rename(f.paths.journal, f.paths.journal+".target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.paths.journal+".target", f.paths.journal); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "wrong-digest" {
				candidate = append(candidate, 'y')
			}
			if err := os.WriteFile(f.paths.current, candidate, 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := New(f.manager.config); err == nil {
				t.Fatal("accepted executable without matching cutover journal")
			}
		})
	}
}

func TestRecoveryAfterInterruptedQuarantineLink(t *testing.T) {
	f := newFixture(t)
	f.fetcher.body = append(append([]byte(nil), f.fetcher.body...), []byte("candidate release")...)
	f.candidate = release(f.candidate.Version, f.fetcher.body)
	if err := os.WriteFile(f.paths.root+"/installed/pb", f.fetcher.body, 0700); err != nil {
		t.Fatal(err)
	}
	f.starter.activateError = errors.New("activation response lost")
	if _, err := f.manager.Activate(context.Background(), f.candidate); err == nil {
		t.Fatal("expected uncertain cutover")
	}
	// Simulate power loss after quarantine was linked, before old bytes replaced
	// current. A pre-existing staged slot must not bypass trust or restoration.
	if err := os.Link(f.paths.current, f.paths.staged); err != nil {
		t.Fatal(err)
	}
	f.fetcher.recoveryError = ErrReleaseRevoked
	if err := f.manager.authorizeStorageRestore(context.Background(), f.active); !errors.Is(err, ErrReleaseRevoked) {
		t.Fatalf("quarantine bypassed recovery policy: %v", err)
	}
	f.fetcher.recoveryError = nil
	if err := f.manager.authorizeStorageRestore(context.Background(), f.active); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.restoreStorage(); err != nil {
		t.Fatal(err)
	}
	if !regularMatches(f.paths.current, f.active.Length, f.active.SHA256) {
		t.Fatal("known installation not restored")
	}
	if !regularMatches(f.paths.staged, f.candidate.Length, f.candidate.SHA256) {
		t.Fatal("failed candidate not quarantined")
	}
}
