package workerupdate

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
)

func installCustomFixture(t *testing.T, f *fixture) Release {
	t.Helper()
	source, err := installsource.Inspect(f.paths.current, "dev-custom", installsource.Custom)
	if err != nil {
		t.Fatal(err)
	}
	active, err := InstalledBaseline(source, f.paths.current)
	if err != nil {
		t.Fatal(err)
	}
	f.active = active
	config := f.manager.config
	config.Active = active
	f.manager, err = New(config)
	if err != nil {
		t.Fatal(err)
	}
	return active
}

func TestCustomInstalledBaselineStartsAndRecoversWithoutPublishedVersion(t *testing.T) {
	f := newFixture(t)
	active := installCustomFixture(t, &f)
	f.fetcher.recoveryError = errors.New("unpublished local version must not query TUF")
	if err := f.manager.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored, err := ActiveReleaseFromJournal(f.paths.journal, active.Version)
	if err != nil || restored.LocalSource == nil || restored.ManifestSHA256 != "" {
		t.Fatalf("local provenance lost: %v", err)
	}
	if err = f.manager.authorizeRecovery(context.Background(), restored, f.paths.current); err != nil {
		t.Fatal(err)
	}
	if f.fetcher.recoveryCalls != 0 {
		t.Fatal("local recovery requested signed release authorization")
	}
	// A published prior release still requires the current signed trust policy.
	official := restored
	official.Version = "2026.09.19.1"
	official.LocalSource = nil
	if err = f.manager.authorizeRecovery(context.Background(), official, f.paths.current); !errors.Is(err, f.fetcher.recoveryError) {
		t.Fatal("official recovery policy bypassed", err)
	}
}

func TestCustomBaselineRejectsTamperingAndCannotBecomeUpdateCandidate(t *testing.T) {
	f := newFixture(t)
	active := installCustomFixture(t, &f)
	if _, err := f.manager.Activate(context.Background(), active); !errors.Is(err, ErrInvalidRelease) {
		t.Fatal("local source accepted as update candidate", err)
	}
	if err := ValidateActivationRelease(active); err == nil {
		t.Fatal("local source accepted as signed activation")
	}
	source := *active.LocalSource
	if err := os.WriteFile(f.paths.current, []byte("changed bytes"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := InstalledBaseline(source, f.paths.current); err == nil {
		t.Fatal("modified installed binary accepted")
	}
	if err := f.manager.authorizeRecovery(context.Background(), active, f.paths.current); err == nil {
		t.Fatal("modified rollback baseline accepted")
	}
}

func TestCustomBaselineOfficialUpdateAndRollbackPreserveProvenance(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "rollback"}[fail], func(t *testing.T) {
			f := newFixture(t)
			active := installCustomFixture(t, &f)
			if fail {
				f.manager.config.Health = &fakeHealth{err: errors.New("candidate unhealthy")}
			}
			result, err := f.manager.Activate(context.Background(), f.candidate)
			if fail && err == nil || !fail && err != nil {
				t.Fatalf("update outcome: %+v %v", result, err)
			}
			journal, readErr := updateflow.Load(f.paths.journal)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if fail {
				if journal.ActiveVersion != active.Version || journal.ActiveSource == nil {
					t.Fatal("rollback lost custom provenance")
				}
			} else if journal.ActiveSource != nil || journal.ActiveVersion != f.candidate.Version {
				t.Fatal("official update retained local provenance")
			}
		})
	}
}

func TestOfficialInstalledBaselineRejectsDowngrade(t *testing.T) {
	f := newFixture(t)
	source, err := installsource.Inspect(f.paths.current, "2026.09.19.1", installsource.Official)
	if err != nil {
		t.Fatal(err)
	}
	active, err := InstalledBaseline(source, f.paths.current)
	if err != nil {
		t.Fatal(err)
	}
	config := f.manager.config
	config.Active = active
	f.manager, err = New(config)
	if err != nil {
		t.Fatal(err)
	}
	candidate := f.candidate
	candidate.Version = "2026.09.18.0"
	if _, err := f.manager.Activate(context.Background(), candidate); !errors.Is(err, ErrInvalidRelease) {
		t.Fatalf("official baseline downgrade accepted: %v", err)
	}
}
