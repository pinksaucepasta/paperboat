package updated

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
)

func TestWindowsNativeInstallRecoveryResumesApprovedOrphan(t *testing.T) {
	journal := testWindowsFeatureJournal()
	var calls []string
	resumed := false
	ops := windowsNativeInstallRecoveryOps{
		validateTerminal: func(context.Context, windowsActivationJournal) error { return nil },
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
		validateTerminal: func(context.Context, windowsActivationJournal) error { return nil },
		load:             func() (windowsActivationJournal, error) { return journal, nil },
		validate:         func(context.Context, windowsActivationJournal) error { return nil },
		owner:            func(windowsActivationJournal) (bool, bool, error) { cancel(); return true, true, nil },
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
				validateTerminal: func(context.Context, windowsActivationJournal) error { return nil },
				load:             func() (windowsActivationJournal, error) { return journal, nil },
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
			finishes := 0
			ops := windowsNativeInstallRecoveryOps{
				validateTerminal: func(context.Context, windowsActivationJournal) error { return nil },
				load:             func() (windowsActivationJournal, error) { return journal, nil },
				validate:         func(context.Context, windowsActivationJournal) error { return nil },
				validateMutation: func(context.Context, windowsActivationJournal) error { return nil },
				owner:            func(windowsActivationJournal) (bool, bool, error) { return registered, false, nil },
				stopUpdater:      func(context.Context) error { t.Fatal("terminal recovery stopped native updater"); return nil },
				resume: func(context.Context, windowsActivationJournal) error {
					t.Fatal("terminal recovery launched old activator")
					return nil
				},
				finishTerminal: func(_ context.Context, j windowsActivationJournal) error {
					if !nativeWindowsJournalRetirable(j) {
						t.Fatal("terminal retirement recut over")
					}
					finishes++
					registered = false
					return nil
				},
			}
			if err := recoverWindowsNativeInstall(context.Background(), ops); err != nil || finishes != 1 {
				t.Fatalf("finishes=%d err=%v", finishes, err)
			}
		})
	}
}

func TestWindowsNativeInstallTerminalFailureRetainsRegistrationForRetry(t *testing.T) {
	journal := testWindowsFeatureJournal()
	journal.Stage = windowsActivationCommitted
	registered := true
	failure := errors.New("native restoration unavailable")
	retired, restores := 0, 0
	ops := windowsNativeInstallRecoveryOps{
		validateTerminal: func(context.Context, windowsActivationJournal) error { return nil },
		load:             func() (windowsActivationJournal, error) { return journal, nil },
		validate:         func(context.Context, windowsActivationJournal) error { return nil },
		validateMutation: func(context.Context, windowsActivationJournal) error { return nil },
		owner:            func(windowsActivationJournal) (bool, bool, error) { return registered, false, nil },
		stopUpdater:      func(context.Context) error { t.Fatal("terminal updater stopped"); return nil },
		resume:           func(context.Context, windowsActivationJournal) error { t.Fatal("old activator launched"); return nil },
		finishTerminal: func(ctx context.Context, j windowsActivationJournal) error {
			return finishWindowsActivatorResult(ctx, j, nil, func(context.Context, windowsActivationJournal) error {
				restores++
				return failure
			}, func() error { retired++; registered = false; return nil })
		},
	}
	if err := recoverWindowsNativeInstall(context.Background(), ops); !errors.Is(err, failure) || !registered || retired != 0 {
		t.Fatalf("failed restoration lost retry ownership: registered=%t retired=%d err=%v", registered, retired, err)
	}
	failure = nil
	if err := recoverWindowsNativeInstall(context.Background(), ops); err != nil || registered || retired != 1 || restores != 2 {
		t.Fatalf("terminal retry: registered=%t retired=%d restores=%d err=%v", registered, retired, restores, err)
	}
}

func TestWindowsNativeInstallTerminalRecoveryRechecksRunningOwner(t *testing.T) {
	journal := testWindowsFeatureJournal()
	journal.Stage = windowsActivationCommitted
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queries := 0
	ops := windowsNativeInstallRecoveryOps{
		validateTerminal: func(context.Context, windowsActivationJournal) error { return nil },
		load:             func() (windowsActivationJournal, error) { return journal, nil },
		validate:         func(context.Context, windowsActivationJournal) error { return nil },
		validateMutation: func(context.Context, windowsActivationJournal) error { return nil },
		owner: func(windowsActivationJournal) (bool, bool, error) {
			queries++
			if queries > 1 {
				cancel()
				return true, true, nil
			}
			return true, false, nil
		},
		finishTerminal: func(context.Context, windowsActivationJournal) error { t.Fatal("running owner stolen"); return nil },
	}
	if err := recoverWindowsNativeInstall(ctx, ops); !errors.Is(err, context.Canceled) {
		t.Fatalf("running-owner cancellation: %v", err)
	}
}

func TestWindowsNativeInstallRecoveryRejectsReplacedTransaction(t *testing.T) {
	journal := testWindowsActivationJournal()
	loads := 0
	ops := windowsNativeInstallRecoveryOps{
		validateTerminal: func(context.Context, windowsActivationJournal) error { return nil },
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
	ops := windowsNativeInstallRecoveryOps{
		validateTerminal: func(context.Context, windowsActivationJournal) error { return nil }, load: func() (windowsActivationJournal, error) { return windowsActivationJournal{}, os.ErrNotExist }, owner: func(windowsActivationJournal) (bool, bool, error) { return true, false, nil }}
	if err := recoverWindowsNativeInstall(context.Background(), ops); !errors.Is(err, errInvalidWindowsActivation) {
		t.Fatal(err)
	}
}

func TestWindowsNativeInstallRecoveryFinishesCandidateSourceBeforeJournalCommit(t *testing.T) {
	journal := testWindowsFeatureJournal()
	journal.Stage = windowsActivationServicesLive
	// Publishing candidate Source does not finish its immutable signed gate.
	// Both startup and installer recovery must resume the orphaned original owner.
	if !windowsActivationNeedsResume(journal, journal.Version, false) {
		t.Fatal("committed candidate hid unfinished signed recovery")
	}
	if windowsActivationNeedsResume(journal, journal.Version, true) {
		t.Fatal("recovery would steal the running signed owner")
	}
	resumed := false
	ops := windowsNativeInstallRecoveryOps{
		validateTerminal: func(context.Context, windowsActivationJournal) error { return nil },
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

func TestWindowsNativeInstallSupersedesOnlyValidatedUnapprovedDownload(t *testing.T) {
	for _, kind := range []string{"unapproved", "registered", "running", "changed_source", "invalid_candidate", "approved"} {
		t.Run(kind, func(t *testing.T) {
			j := testWindowsFeatureJournal()
			j.Stage = windowsActivationAwaitingApproval
			j.ApprovedCandidateID = ""
			source := installsource.Source{Version: j.PreviousVersion, Platform: "windows", Architecture: j.Architecture, SHA256: j.PreviousBinary.SHA256, Length: j.PreviousBinary.Length, Distribution: installsource.Custom}
			j.PreviousSource = &source
			if kind == "changed_source" {
				source.SHA256 = strings.Repeat("f", 64)
			}
			if kind == "invalid_candidate" {
				j.Candidate.ID = "bad"
			}
			if kind == "approved" {
				j.ApprovedCandidateID = j.Candidate.ID
			}
			validated := false
			mutation := func(context.Context) error { t.Fatal("unapproved download changed services"); return nil }
			ops := windowsNativeInstallRecoveryOps{
				validateTerminal: func(context.Context, windowsActivationJournal) error { return nil },
				load:             func() (windowsActivationJournal, error) { return j, nil },
				validate: func(context.Context, windowsActivationJournal) error {
					validated = true
					return validateWindowsNativeInstallPreviousSource(source, j)
				},
				owner: func(windowsActivationJournal) (bool, bool, error) {
					return kind == "registered", kind == "running", nil
				},
				stopUpdater: mutation,
				resume:      func(context.Context, windowsActivationJournal) error { t.Fatal("download was activated"); return nil },
				finishTerminal: func(context.Context, windowsActivationJournal) error {
					t.Fatal("download was retired as committed")
					return nil
				},
			}
			err := recoverWindowsNativeInstall(context.Background(), ops)
			if kind == "unapproved" {
				if err != nil || !validated {
					t.Fatalf("validated download cannot be superseded: %v", err)
				}
			} else if err == nil {
				t.Fatalf("%s accepted", kind)
			}
		})
	}
}

func TestWindowsOfflineNativeInstallDoesNotEnterLiveRecovery(t *testing.T) {
	for _, stage := range []windowsActivationStage{windowsActivationSwitching, windowsActivationCommitted, windowsActivationAwaitingApproval} {
		t.Run(string(stage), func(t *testing.T) {
			journal := testWindowsFeatureJournal()
			journal.Stage = stage
			if stage == windowsActivationAwaitingApproval {
				journal.ApprovedCandidateID = ""
				journal.Force = false
			}
			forbidden := func(context.Context, windowsActivationJournal) error {
				t.Fatal("offline operation entered policy/network or mutation path")
				return nil
			}
			ops := windowsNativeInstallRecoveryOps{
				validateTerminal: func(context.Context, windowsActivationJournal) error { return nil }, offline: true, load: func() (windowsActivationJournal, error) { return journal, nil }, owner: func(windowsActivationJournal) (bool, bool, error) { return false, false, nil }, validate: forbidden, validateMutation: forbidden, finishTerminal: forbidden, resume: forbidden, stopUpdater: func(context.Context) error { t.Fatal("offline operation stopped updater"); return nil }}
			err := recoverWindowsNativeInstall(context.Background(), ops)
			if stage == windowsActivationSwitching {
				if err != ErrWindowsActivationUnavailable {
					t.Fatalf("pending offline activation error=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWindowsNativeInstallTerminalRequiresSourceAndGateProof(t *testing.T) {
	journal := testWindowsFeatureJournal()
	journal.Stage = windowsActivationCommitted
	denied := errors.New("protected native source or original gate outcome is uncertain")
	checked := false
	ops := windowsNativeInstallRecoveryOps{
		load:     func() (windowsActivationJournal, error) { return journal, nil },
		validate: func(context.Context, windowsActivationJournal) error { return nil },
		owner:    func(windowsActivationJournal) (bool, bool, error) { return false, false, nil },
		validateTerminal: func(_ context.Context, actual windowsActivationJournal) error {
			checked = true
			if actual.Candidate != journal.Candidate || actual.TransactionID != journal.TransactionID {
				t.Fatal("terminal proof changed original approval")
			}
			return denied
		},
	}
	if err := recoverWindowsNativeInstall(context.Background(), ops); !errors.Is(err, denied) || !checked {
		t.Fatalf("terminal absence bypassed source/gate proof: checked=%t err=%v", checked, err)
	}
	ops.validateTerminal = nil
	if err := recoverWindowsNativeInstall(context.Background(), ops); err == nil {
		t.Fatal("terminal completion accepted without native proof consumer")
	}
	ops.validateTerminal = func(context.Context, windowsActivationJournal) error { return nil }
	if err := recoverWindowsNativeInstall(context.Background(), ops); err != nil {
		t.Fatalf("verified terminal completion rejected: %v", err)
	}
	// Clearing a protected receipt is not evidence that the exact helper finished.
	ops.load = func() (windowsActivationJournal, error) { return windowsActivationJournal{}, os.ErrNotExist }
	ops.owner = func(windowsActivationJournal) (bool, bool, error) { return true, false, nil }
	if err := recoverWindowsNativeInstall(context.Background(), ops); err == nil {
		t.Fatal("orphan helper accepted after receipt disappeared")
	}
}
