//go:build darwin || linux

package updated

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"strings"
)

func TestUnixNativeInstallRetiresIdleStateAndRestoresOnRollback(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected native installer state requires root")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "transaction.json")
	j := updateflow.Journal{Schema: updateflow.SchemaV1, TransactionID: "native-install-fixture", Stage: updateflow.StageIdle, ActiveVersion: "2026.09.19.0", BootID: "hostd", StageUpdatedAt: time.Now().UTC(), ActiveDigest: strings.Repeat("a", 64), ActiveLength: 1, ActiveHostdAPIMin: 1, ActiveHostdAPIMax: 1, ActiveRuntimeAPIMin: 1, ActiveRuntimeAPIMax: 1}
	if err := updateflow.Write(path, j, 0, 0); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tuf := filepath.Join(root, "trust-proof")
	if err = os.WriteFile(tuf, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := beginNativeInstallFixture(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old transaction retained: %v", err)
	}
	lock.Close()
	if pending, err := nativeInstallPending(root); err != nil || !pending {
		t.Fatal("install not fenced", err)
	}
	if err = RollbackUnixNativeInstall(root); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("prior transaction not restored exactly", err)
	}
	lock, err = beginNativeInstallFixture(root)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	if err = CommitUnixNativeInstall(root); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("committed installation retained old baseline")
	}
	if data, err := os.ReadFile(tuf); err != nil || string(data) != "unchanged" {
		t.Fatal("trust state changed", err)
	}
}

func TestUnixNativeInstallRejectsPendingActivationWithoutMutation(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected native installer state requires root")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	h := &unixActivationHandoff{Schema: unixHandoffSchema, Previous: workerupdate.Release{Version: "2026.09.19.0", SHA256: strings.Repeat("a", 64), Length: 1}, Candidate: "2026.09.19.1"}
	if err := writeUnixHandoff(root, h); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(unixHandoffPath(root))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := beginNativeInstallFixture(root)
	if lock != nil {
		lock.Close()
	}
	if !errors.Is(err, ErrActivationPending) {
		t.Fatal("pending update accepted", err)
	}
	after, err := os.ReadFile(unixHandoffPath(root))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("pending recovery changed", err)
	}
	if pending, err := nativeInstallPending(root); err != nil || pending {
		t.Fatal("failed installation left marker", err)
	}
}

func beginNativeInstallFixture(root string) (io.Closer, error) {
	lock, err := LockUnixNativeInstall(context.Background(), root)
	if err != nil {
		return nil, err
	}
	if err = PrepareUnixNativeInstall(root); err != nil {
		lock.Close()
		return nil, err
	}
	return lock, nil
}

func TestUnixNativeInstallSupersedesOnlyUnapprovedDownload(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected native installer state requires root")
	}
	for _, approved := range []bool{false, true} {
		t.Run(fmt.Sprint(approved), func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "transaction.json")
			j := updateflow.Journal{Schema: updateflow.SchemaV1, TransactionID: "native-install-candidate", Stage: updateflow.StageAwaitingApproval, ActiveVersion: "2026.09.30.10", BootID: "hostd", StageUpdatedAt: time.Now().UTC(), ActiveDigest: strings.Repeat("a", 64), ActiveLength: 1, ActiveHostdAPIMin: 1, ActiveHostdAPIMax: 1, ActiveRuntimeAPIMin: 1, ActiveRuntimeAPIMax: 1,
				CandidateID: strings.Repeat("b", 64), ArtifactDigest: strings.Repeat("c", 64), ArtifactLength: 1, ArtifactPlatform: "linux", ArtifactArchitecture: "amd64", CandidateVersion: "2026.09.30.12", CandidateDigest: strings.Repeat("c", 64), CandidateLength: 1, StagedPath: filepath.Join(root, "staged")}
			if approved {
				j.ApprovedCandidateID = j.CandidateID
			}
			if err := updateflow.Write(path, j, 0, 0); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lock, err := beginNativeInstallFixture(root)
			if lock != nil {
				defer lock.Close()
			}
			if approved {
				if !errors.Is(err, ErrActivationPending) {
					t.Fatalf("approved candidate accepted: %v", err)
				}
				after, readErr := os.ReadFile(path)
				if readErr != nil || !bytes.Equal(before, after) {
					t.Fatal("approved journal changed", readErr)
				}
				if pending, err := nativeInstallPending(root); pending || err != nil {
					t.Fatal("rejected install left marker", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("candidate not fenced", err)
			}
			if err = RollbackUnixNativeInstall(root); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("unapproved candidate not restored exactly", err)
			}
			if err = PrepareUnixNativeInstall(root); err != nil {
				t.Fatal(err)
			}
			if err = CommitUnixNativeInstall(root); err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("superseded journal retained", err)
			}
		})
	}
}

func TestUnixNativeInstallLockWaitsForTransientOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected native installer state requires root")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	owner, err := unixActivationLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	acquired := make(chan error, 1)
	go func() {
		lock, err := LockUnixNativeInstall(ctx, root)
		if lock != nil {
			lock.Close()
		}
		acquired <- err
	}()
	select {
	case err := <-acquired:
		t.Fatalf("returned while lock held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-acquired; err != nil {
		t.Fatal(err)
	}
}

func TestUnixNativeInstallLockCancellationAndDeadlineLeaveOwnerIntact(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected native installer state requires root")
	}
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			owner, err := unixActivationLock(root)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			ctx, cancel := context.WithCancel(context.Background())
			want := context.Canceled
			if deadline {
				ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
				want = context.DeadlineExceeded
			} else {
				cancel()
			}
			defer cancel()
			lock, err := LockUnixNativeInstall(ctx, root)
			if lock != nil {
				lock.Close()
				t.Fatal("acquired occupied lock")
			}
			if !errors.Is(err, want) || !errors.Is(err, ErrActivationPending) {
				t.Fatalf("wrong cancellation: %v", err)
			}
			if other, err := unixActivationLock(root); !errors.Is(err, ErrActivationPending) {
				if other != nil {
					other.Close()
				}
				t.Fatalf("existing owner lost exclusion: %v", err)
			}
		})
	}
}

func TestUnixSuppliedInstallSupersedesOnlyUntouchedBoundActivation(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected activation state requires root")
	}
	for _, scenario := range []string{"queued", "idle_cleanup_retry", "other_notice", "helper_started", "participants_touched", "wrong_approval", "changed_binary", "retire_failed", "cutover"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			executable := filepath.Join(root, "installed-pb")
			content := make([]byte, 32)
			if runtime.GOOS == "linux" {
				copy(content, "\x7fELF")
				content[4] = 2
				content[5] = 1
				machine := uint16(62)
				if runtime.GOARCH == "arm64" {
					machine = 183
				}
				binary.LittleEndian.PutUint16(content[18:20], machine)
			} else {
				binary.LittleEndian.PutUint32(content[:4], 0xfeedfacf)
				cpu := uint32(0x01000007)
				if runtime.GOARCH == "arm64" {
					cpu = 0x0100000c
				}
				binary.LittleEndian.PutUint32(content[4:8], cpu)
			}
			sum := sha256.Sum256(content)
			source := installsource.Source{Version: "2026.09.30.10", Platform: runtime.GOOS, Architecture: runtime.GOARCH, SHA256: hex.EncodeToString(sum[:]), Length: int64(len(content)), Distribution: installsource.Official, AutomaticUpdates: true}
			if err := os.WriteFile(executable, content, 0755); err != nil {
				t.Fatal(err)
			}
			approval := strings.Repeat("b", 64)
			h := &unixActivationHandoff{Schema: unixHandoffSchema, Previous: workerupdate.Release{Version: source.Version, SHA256: source.SHA256, Length: source.Length}, Candidate: "2026.09.30.12", ApprovalID: approval}
			j := updateflow.Journal{Schema: updateflow.SchemaV1, TransactionID: "supplied-supersession", BootID: "hostd", Stage: updateflow.StageAwaitingApproval, StageUpdatedAt: time.Now().UTC(), ActiveVersion: source.Version, ActiveDigest: source.SHA256, ActiveLength: source.Length, ActiveHostdAPIMin: 1, ActiveHostdAPIMax: 1, ActiveRuntimeAPIMin: 1, ActiveRuntimeAPIMax: 1, CandidateID: approval, ApprovedCandidateID: approval, ArtifactDigest: strings.Repeat("c", 64), ArtifactLength: 1, ArtifactPlatform: runtime.GOOS, ArtifactArchitecture: runtime.GOARCH, CandidateVersion: h.Candidate, CandidateDigest: strings.Repeat("c", 64), CandidateLength: 1, StagedPath: filepath.Join(root, "staged"), NativeActivation: true}
			switch scenario {
			case "idle_cleanup_retry":
				j.Stage = updateflow.StageIdle
				j.CandidateID = ""
				j.ApprovedCandidateID = ""
				j.CandidateVersion = ""
				j.CandidateDigest = ""
				j.CandidateLength = 0
				j.ArtifactDigest = ""
				j.ArtifactLength = 0
				j.ArtifactPlatform = ""
				j.ArtifactArchitecture = ""
				j.StagedPath = ""
				j.NativeActivation = false
			case "helper_started":
				h.Started = true
			case "participants_touched":
				h.ParticipantsTouched = true
			case "wrong_approval":
				h.ApprovalID = strings.Repeat("d", 64)
			case "changed_binary":
				if err := os.WriteFile(executable, []byte("changed bytes"), 0755); err != nil {
					t.Fatal(err)
				}
			case "cutover":
				j.Stage = updateflow.StageCutover
			}
			if err := writeUnixHandoff(root, h); err != nil {
				t.Fatal(err)
			}
			journalPath := filepath.Join(root, "transaction.json")
			if err := updateflow.Write(journalPath, j, 0, 0); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			noticeID := approval
			if scenario == "other_notice" {
				noticeID = strings.Repeat("e", 64)
			}
			noticePath := filepath.Join(root, "owner-maintenance.json")
			if _, err := autoupdate.PlanOwnerMaintenance(noticePath, noticeID, h.Candidate, 60, time.Now()); err != nil {
				t.Fatal(err)
			}
			controller := &controllerFixture{}
			if scenario == "retire_failed" {
				controller.retireErr = errors.New("native retirement failed")
			}
			lock, err := LockUnixNativeInstall(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			err = SupersedeUntouchedUnixActivation(context.Background(), root, executable, source, controller)
			success := scenario == "queued" || scenario == "idle_cleanup_retry" || scenario == "other_notice"
			if !success {
				if err == nil {
					t.Fatal("unsafe activation superseded")
				}
				after, readErr := os.ReadFile(journalPath)
				if readErr != nil || !bytes.Equal(before, after) {
					t.Fatal("refusal changed journal", readErr)
				}
				if _, readErr := readUnixHandoff(root); readErr != nil {
					t.Fatal(readErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			after, err := updateflow.Load(journalPath)
			if err != nil || after.Stage != updateflow.StageIdle || after.ApprovedCandidateID != "" || after.ActiveDigest != source.SHA256 {
				t.Fatal("activation not safely canceled", err)
			}
			if h, err := readUnixHandoff(root); err != nil || h != nil {
				t.Fatal("handoff retained", err)
			}
			notice, err := autoupdate.LoadOwnerMaintenance(noticePath)
			if err != nil || (scenario == "other_notice" && (notice == nil || notice.CandidateID != noticeID)) || (scenario != "other_notice" && notice != nil) {
				t.Fatal("canceled candidate notice not cleared exactly", err)
			}
			if err := source.Verify(executable); err != nil {
				t.Fatal("installed bytes changed", err)
			}
			if err := PrepareUnixNativeInstall(root); err != nil {
				t.Fatal("supplied install still blocked", err)
			}
			if err := RollbackUnixNativeInstall(root); err != nil {
				t.Fatal(err)
			}
			after, err = updateflow.Load(journalPath)
			if err != nil || after.Stage != updateflow.StageIdle || after.ApprovedCandidateID != "" {
				t.Fatal("rollback resurrected approval", err)
			}
		})
	}
}
