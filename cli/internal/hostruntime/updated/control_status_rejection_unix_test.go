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

// status keeps answering for a downloaded update that stopped verifying, and says so when the reason is one a user must know:
// an unsafe file is reported with the version and reason; a payload that was merely replaced is not.
func TestControlStatusReportsAnUnsafeDownloadedUpdateButNotAReplacedOne(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected activation state requires root")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
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
	staged := filepath.Join(root, "staged")
	manager, err := workerupdate.New(workerupdate.Config{StatePath: journal, Binary: binary, BinaryStaged: staged, Active: active,
		OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), Fetcher: statusFetcher{}, Hostd: statusHostd{}, Health: statusHealth{}, Gate: statusGate{},
		ApplyPrepared: func(context.Context, workerupdate.PreparedActivation) (installsource.Source, error) {
			return installsource.Source{}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	approval := strings.Repeat("b", 64)
	j := updateflow.Journal{Schema: updateflow.SchemaV1, TransactionID: "unsafe", BootID: "hostd", Stage: updateflow.StageAwaitingApproval, StageUpdatedAt: time.Now().UTC(), ActiveVersion: active.Version, ActiveDigest: active.SHA256, ActiveLength: active.Length, ActiveHostdAPIMin: 1, ActiveHostdAPIMax: 1, ActiveRuntimeAPIMin: 1, ActiveRuntimeAPIMax: 1, CandidateID: approval, ArtifactDigest: strings.Repeat("c", 64), ArtifactLength: 1, ArtifactPlatform: runtime.GOOS, ArtifactArchitecture: runtime.GOARCH, CandidateVersion: "2026.10.28.2", CandidateDigest: strings.Repeat("c", 64), CandidateLength: 1, StagedPath: staged}
	if err := updateflow.Write(journal, j, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	s := &Service{manager: manager, config: Config{StateRoot: root}}
	status := func() ControlResponse {
		t.Helper()
		response, err := s.controlRequestWithRequest(context.Background(), ControlRequest{Schema: ControlProtocolV1, Operation: "status"})
		if err != nil || response.Candidate != nil {
			t.Fatalf("status must answer without offering the candidate: %+v %v", response, err)
		}
		return response
	}
	// A payload that does not match its digest: replaced, benign, not reported.
	if err := os.WriteFile(staged, []byte("replaced"), 0600); err != nil {
		t.Fatal(err)
	}
	if response := status(); response.CandidateRejected != nil {
		t.Fatalf("a replaced payload was reported: %+v", response.CandidateRejected)
	}
	// A payload anyone may write is reported, with the version and the reason.
	if err := os.Chmod(staged, 0o666); err != nil {
		t.Fatal(err)
	}
	response := status()
	if response.CandidateRejected == nil || response.CandidateRejected.Reason != "unsafe_file" || response.CandidateRejected.Version != j.CandidateVersion {
		t.Fatalf("an unsafe downloaded update was not reported: %+v", response.CandidateRejected)
	}
}
