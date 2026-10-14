//go:build windows

package updated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

func TestWindowsPreparedCandidateRevalidatesActualExecutable(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "candidate.exe")
	if err = os.WriteFile(path, body, 0700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	j := testWindowsActivationJournal()
	j.Stage = windowsActivationAwaitingApproval
	j.ApprovedCandidateID = ""
	j.Architecture = runtime.GOARCH
	j.Runtime = windowsActivationComponent{Path: path, SHA256: hex.EncodeToString(digest[:]), Length: int64(len(body))}
	j.Candidate.SHA256 = j.Runtime.SHA256
	j.Candidate.Length = j.Runtime.Length
	j.Candidate.Architecture = runtime.GOARCH
	if err = verifyWindowsPreparedCandidate(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	body[len(body)-1] ^= 1
	if err = os.WriteFile(path, body, 0700); err != nil {
		t.Fatal(err)
	}
	if err = verifyWindowsPreparedCandidate(context.Background(), j); !errors.Is(err, workerupdate.ErrPreparedCandidate) {
		t.Fatalf("altered bytes accepted: %v", err)
	}
}
