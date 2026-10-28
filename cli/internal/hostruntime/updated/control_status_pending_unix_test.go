//go:build darwin || linux

package updated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

type statusFetcher struct{ workerupdate.Fetcher }
type statusHostd struct{ workerupdate.Hostd }
type statusHealth struct{ workerupdate.HealthChecker }
type statusGate struct{ workerupdate.ActivationGate }

// dadape rolled back 2026.10.28.1→.2: the restarted daemon's readiness probe reads updater status, and status
// failed with candidate_changed on every probe because the approved payload no longer verified mid-activation.
func TestControlStatusAnswersDuringApprovedActivationWithUnverifiablePayload(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected activation state requires root")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil { // the manager refuses group-writable storage (umask 002)
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "pb")
	if err := os.WriteFile(binary, content, 0755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	active := workerupdate.Release{Version: "2026.10.28.1", Platform: runtime.GOOS, Architecture: runtime.GOARCH, SHA256: hex.EncodeToString(sum[:]), Length: int64(len(content)), HostdAPIMin: 1, HostdAPIMax: 1, RuntimeAPIMin: 1, RuntimeAPIMax: 1}
	journal := filepath.Join(root, "transaction.json")
	manager, err := workerupdate.New(workerupdate.Config{StatePath: journal, Binary: binary, BinaryStaged: filepath.Join(root, "staged"), Active: active,
		OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), Fetcher: statusFetcher{}, Hostd: statusHostd{}, Health: statusHealth{}, Gate: statusGate{},
		ApplyPrepared: func(context.Context, workerupdate.PreparedActivation) (installsource.Source, error) {
			return installsource.Source{}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	approval := strings.Repeat("b", 64)
	j := updateflow.Journal{Schema: updateflow.SchemaV1, TransactionID: "dadape", BootID: "hostd", Stage: updateflow.StageAwaitingApproval, StageUpdatedAt: time.Now().UTC(), ActiveVersion: active.Version, ActiveDigest: active.SHA256, ActiveLength: active.Length, ActiveHostdAPIMin: 1, ActiveHostdAPIMax: 1, ActiveRuntimeAPIMin: 1, ActiveRuntimeAPIMax: 1, CandidateID: approval, ApprovedCandidateID: approval, ArtifactDigest: strings.Repeat("c", 64), ArtifactLength: 1, ArtifactPlatform: runtime.GOOS, ArtifactArchitecture: runtime.GOARCH, CandidateVersion: "2026.10.28.2", CandidateDigest: strings.Repeat("c", 64), CandidateLength: 1, StagedPath: filepath.Join(root, "staged"), NativeActivation: true}
	if err := updateflow.Write(journal, j, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	s := &Service{manager: manager, config: Config{StateRoot: root}}
	staged := filepath.Join(root, "staged")
	status := func() (ControlResponse, error) {
		return s.controlRequestWithRequest(context.Background(), ControlRequest{Schema: ControlProtocolV1, Operation: "status"})
	}
	// Before activation a staged payload that does not match its digest is reported, and so is a missing one.
	if err := os.WriteFile(staged, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := status(); controlErrorCode(err) != "candidate_changed" {
		t.Fatalf("status hid a tampered candidate: %v", err)
	}
	h := &unixActivationHandoff{Schema: unixHandoffSchema, Previous: active, Candidate: j.CandidateVersion, ApprovalID: approval}
	if err := writeUnixHandoff(root, h); err != nil {
		t.Fatal(err)
	}
	// Activation in progress but the payload still in place and mismatched: still reported.
	if _, err := status(); controlErrorCode(err) != "candidate_changed" {
		t.Fatalf("status hid a tampered candidate during activation: %v", err)
	}
	// The native install moves the payload: status answers for the activation's readiness probe.
	if err := os.Remove(staged); err != nil {
		t.Fatal(err)
	}
	response, err := status()
	if err != nil || !response.Pending || response.UpdaterVersion == "" || response.Candidate != nil {
		t.Fatalf("status during the approved activation: %+v %v", response, err)
	}
	// A handoff for another candidate does not excuse the missing payload.
	h.Candidate = "2026.10.28.9"
	if err := writeUnixHandoff(root, h); err != nil {
		t.Fatal(err)
	}
	if _, err := status(); controlErrorCode(err) != "candidate_changed" {
		t.Fatalf("status accepted a missing payload for another activation: %v", err)
	}
}
