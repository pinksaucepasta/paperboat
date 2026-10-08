//go:build windows

package updated

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

func TestWindowsNativeInstallRecoveryResumesApprovedOrphan(t *testing.T) {
	journal := testWindowsFeatureJournal()
	var calls []string
	resumed := false
	ops := windowsNativeInstallRecoveryOps{
		load: func() (windowsActivationJournal, error) {
			if resumed {
				journal.Stage = windowsActivationCommitted
			}
			return journal, nil
		},
		validate:         func(context.Context, windowsActivationJournal) error { calls = append(calls, "validate"); return nil },
		validateMutation: func(context.Context, windowsActivationJournal) error { calls = append(calls, "pins"); return nil },
		owner:            func(windowsActivationJournal) (bool, bool, error) { return !resumed, false, nil },
		stopUpdater:      func(context.Context) error { calls = append(calls, "stop_updated"); return nil },
		resume: func(context.Context, windowsActivationJournal) error {
			calls = append(calls, "resume")
			resumed = true
			return nil
		},
	}
	if err := recoverWindowsNativeInstall(context.Background(), ops); err != nil {
		t.Fatal(err)
	}
	if want := []string{"validate", "pins", "stop_updated", "resume"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%v want=%v", calls, want)
	}
}

func TestWindowsNativeInstallRecoveryPreservesRunningOwnerAndCancellation(t *testing.T) {
	journal := testWindowsActivationJournal()
	ctx, cancel := context.WithCancel(context.Background())
	ops := windowsNativeInstallRecoveryOps{
		load:     func() (windowsActivationJournal, error) { return journal, nil },
		validate: func(context.Context, windowsActivationJournal) error { return nil },
		owner:    func(windowsActivationJournal) (bool, bool, error) { cancel(); return true, true, nil },
		validateMutation: func(context.Context, windowsActivationJournal) error {
			t.Fatal("running activator pins mutated")
			return nil
		},
		stopUpdater: func(context.Context) error { t.Fatal("running owner's updater stopped"); return nil },
		resume:      func(context.Context, windowsActivationJournal) error { t.Fatal("running owner stolen"); return nil },
	}
	if err := recoverWindowsNativeInstall(ctx, ops); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if journal.Stage != windowsActivationStaged {
		t.Fatal("cancellation changed journal")
	}
}

func TestWindowsNativeInstallRecoveryRejectsUntrustedBeforeMutation(t *testing.T) {
	for _, kind := range []string{"unapproved", "invalid_candidate", "config", "pins", "owner", "start"} {
		t.Run(kind, func(t *testing.T) {
			journal := testWindowsActivationJournal()
			rejected := errors.New("rejected boundary")
			mutations := 0
			if kind == "unapproved" {
				journal.ApprovedCandidateID = ""
				journal.Stage = windowsActivationAwaitingApproval
			}
			if kind == "invalid_candidate" {
				journal.Candidate.ID = "invalid"
			}
			ops := windowsNativeInstallRecoveryOps{
				load: func() (windowsActivationJournal, error) { return journal, nil },
				validate: func(context.Context, windowsActivationJournal) error {
					if kind == "config" {
						return rejected
					}
					return nil
				},
				validateMutation: func(context.Context, windowsActivationJournal) error {
					if kind == "pins" {
						return rejected
					}
					return nil
				},
				owner: func(windowsActivationJournal) (bool, bool, error) {
					if kind == "owner" {
						return true, false, rejected
					}
					return true, false, nil
				},
				stopUpdater: func(context.Context) error { mutations++; return nil },
				resume: func(context.Context, windowsActivationJournal) error {
					if kind == "start" {
						return rejected
					}
					t.Fatal("untrusted transaction resumed")
					return nil
				},
			}
			if err := recoverWindowsNativeInstall(context.Background(), ops); err == nil {
				t.Fatal("untrusted recovery accepted")
			}
			want := 0
			if kind == "start" {
				want = 1
			}
			if mutations != want {
				t.Fatalf("mutations=%d want=%d", mutations, want)
			}
			if journal.Stage != windowsActivationAwaitingApproval && journal.Stage != windowsActivationStaged {
				t.Fatal("failed recovery changed journal")
			}
		})
	}
}

func TestWindowsNativeInstallRecoveryRetiresTerminalWithoutRecutover(t *testing.T) {
	for _, stage := range []windowsActivationStage{windowsActivationCommitted, windowsActivationRolledBack} {
		t.Run(string(stage), func(t *testing.T) {
			journal := testWindowsActivationJournal()
			journal.Stage = stage
			registered := true
			starts := 0
			ops := windowsNativeInstallRecoveryOps{
				load:             func() (windowsActivationJournal, error) { return journal, nil },
				validate:         func(context.Context, windowsActivationJournal) error { return nil },
				validateMutation: func(context.Context, windowsActivationJournal) error { return nil },
				owner:            func(windowsActivationJournal) (bool, bool, error) { return registered, false, nil },
				stopUpdater:      func(context.Context) error { return nil },
				resume: func(_ context.Context, j windowsActivationJournal) error {
					if !nativeWindowsJournalRetirable(j) {
						t.Fatal("terminal retirement recut over")
					}
					starts++
					registered = false
					return nil
				},
			}
			if err := recoverWindowsNativeInstall(context.Background(), ops); err != nil || starts != 1 {
				t.Fatalf("starts=%d err=%v", starts, err)
			}
		})
	}
}

func TestWindowsNativeInstallRecoveryRejectsReplacedTransaction(t *testing.T) {
	journal := testWindowsActivationJournal()
	loads := 0
	ops := windowsNativeInstallRecoveryOps{
		load: func() (windowsActivationJournal, error) {
			loads++
			if loads > 1 {
				journal.TransactionID = "ffffffffffffffffffffffffffffffff"
			}
			return journal, nil
		},
		validate: func(context.Context, windowsActivationJournal) error { return nil },
		owner:    func(windowsActivationJournal) (bool, bool, error) { return true, true, nil },
	}
	if err := recoverWindowsNativeInstall(context.Background(), ops); !errors.Is(err, errInvalidWindowsActivation) {
		t.Fatalf("err=%v", err)
	}
}

func TestWindowsNativeInstallRecoveryRejectsOrphanRegistrationWithoutJournal(t *testing.T) {
	ops := windowsNativeInstallRecoveryOps{load: func() (windowsActivationJournal, error) { return windowsActivationJournal{}, os.ErrNotExist }, owner: func(windowsActivationJournal) (bool, bool, error) { return true, false, nil }}
	if err := recoverWindowsNativeInstall(context.Background(), ops); !errors.Is(err, errInvalidWindowsActivation) {
		t.Fatal(err)
	}
}

func TestWindowsNativeInstallPinRequiresExactDeclaredBytesAndArguments(t *testing.T) {
	expected := windowsServiceTarget{Executable: `C:\Paperboat\versions\2026.10.08.32\pb.exe`, SHA256: "digest", Length: 123, Arguments: []string{"daemon", "__runtime-updated", "--instance", "owner"}}
	actual := expected
	identity := service.WindowsExecutableIdentity{Executable: expected.Executable, SHA256: expected.SHA256, Length: expected.Length}
	if !windowsNativeInstallPinMatches(expected, actual, identity) {
		t.Fatal("valid pin rejected")
	}
	wrong := identity
	wrong.SHA256 = "other"
	if windowsNativeInstallPinMatches(expected, actual, wrong) {
		t.Fatal("wrong bytes accepted")
	}
	actual.Arguments = append([]string(nil), expected.Arguments...)
	actual.Arguments[3] = "another-owner"
	if windowsNativeInstallPinMatches(expected, actual, identity) {
		t.Fatal("wrong owner accepted")
	}
}

func TestWindowsNativeInstallRecoveryFinishesCandidateSourceBeforeJournalCommit(t *testing.T) {
	journal := testWindowsFeatureJournal()
	journal.Stage = windowsActivationServicesLive
	// The normal startup predicate protects a candidate updater's own journal.
	// Installation has stopped that updater, so it uses the verified launch body.
	if windowsActivationNeedsResume(journal, journal.Version, false) {
		t.Fatal("startup candidate guard changed")
	}
	resumed := false
	ops := windowsNativeInstallRecoveryOps{
		load: func() (windowsActivationJournal, error) {
			if resumed {
				journal.Stage = windowsActivationCommitted
			}
			return journal, nil
		},
		validate:         func(context.Context, windowsActivationJournal) error { return nil },
		validateMutation: func(context.Context, windowsActivationJournal) error { return nil },
		owner:            func(windowsActivationJournal) (bool, bool, error) { return !resumed, false, nil },
		stopUpdater:      func(context.Context) error { return nil },
		resume: func(_ context.Context, j windowsActivationJournal) error {
			if j.Stage != windowsActivationServicesLive {
				t.Fatal("recovery fabricated journal stage")
			}
			resumed = true
			return nil
		},
	}
	if err := recoverWindowsNativeInstall(context.Background(), ops); err != nil || !resumed {
		t.Fatalf("resumed=%t err=%v", resumed, err)
	}
}

func TestWindowsNativeInstallJournalBindsProtectedSourceBytesAndPlatform(t *testing.T) {
	journal := testWindowsFeatureJournal()
	config := WindowsConfig{Architecture: journal.Architecture, Source: installsource.Source{Version: journal.PreviousVersion, Platform: "windows", Architecture: journal.Architecture, SHA256: journal.PreviousBinary.SHA256, Length: journal.PreviousBinary.Length}}
	if err := validateWindowsNativeInstallJournalBinding(config, journal); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"version", "hash", "length", "architecture", "platform"} {
		bad := config
		switch kind {
		case "version":
			bad.Source.Version = "2026.10.08.99"
		case "hash":
			bad.Source.SHA256 = "wrong"
		case "length":
			bad.Source.Length++
		case "architecture":
			bad.Source.Architecture = "arm64"
		case "platform":
			bad.Source.Platform = "linux"
		}
		if err := validateWindowsNativeInstallJournalBinding(bad, journal); !errors.Is(err, errInvalidWindowsActivation) {
			t.Fatalf("%s accepted: %v", kind, err)
		}
	}
	config.Source.Version = journal.Version
	config.Source.SHA256 = journal.Runtime.SHA256
	config.Source.Length = journal.Runtime.Length
	if err := validateWindowsNativeInstallJournalBinding(config, journal); err != nil {
		t.Fatalf("Source-before-journal commit rejected: %v", err)
	}
}
