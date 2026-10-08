//go:build darwin || linux

package updated

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

func TestNativeManualCommitUsesCommittedBinary(t *testing.T) {
	binary := os.Getenv("PAPERBOAT_TEST_MANUAL_BINARY")
	if binary == "" {
		t.Skip("native packaged binary not selected")
	}
	if os.Geteuid() != 0 {
		t.Fatal("protected committed binary fixture requires root")
	}
	body, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	release := workerupdate.Release{Version: "native-candidate", SHA256: fmt.Sprintf("%x", sha256.Sum256(body)), Length: int64(len(body))}
	directory := t.TempDir()
	section := filepath.Join(directory, "man1")
	if err := os.Mkdir(section, 0755); err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(section, "pb.1")
	previous := []byte("prior release manual")
	if err := os.WriteFile(page, previous, 0644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	calls := 0
	s := &Service{config: Config{Binary: binary, RefreshManuals: func(ctx context.Context) error {
		calls++
		cmd := exec.CommandContext(ctx, binary, "--no-customization", "__man-pages", "--directory", directory)
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		return cmd.Run()
	}}}
	inner := &manualInnerGate{}
	gate := s.manualCommitGate(inner)
	req := workerupdate.GateRequest{Candidate: release}
	if err := gate.Rollback(ctx, req); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(page); !bytes.Equal(got, previous) || calls != 0 {
		t.Fatal("precommit rollback changed manuals")
	}
	// A blocked target leaves the postcommit refresh pending, without undoing
	// runtime commit. The same gate can be retried by journal recovery.
	if err := os.Remove(page); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(directory, "missing"), page); err != nil {
		t.Fatal(err)
	}
	if err := gate.Commit(ctx, req); err == nil {
		t.Fatal("manual failure was reported as complete")
	}
	if err := os.Remove(page); err != nil {
		t.Fatal(err)
	}
	if err := gate.Commit(ctx, req); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(page)
	if err != nil || !bytes.HasPrefix(got, []byte(".\\\" Paperboat managed manual v1\n")) || calls != 2 || inner.commits != 2 || inner.rollbacks != 1 {
		t.Fatalf("commit/retry did not install candidate manual: calls=%d commits=%d rollbacks=%d err=%v", calls, inner.commits, inner.rollbacks, err)
	}
}
