//go:build windows

package hostservice

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

type recordingReconciler struct {
	calls     int
	keys      []string
	principal string
	revoked   []string
}

func (r *recordingReconciler) ReconcileAuthorizedKeys(_ context.Context, keys []string, principal string, revoked []string) (bool, error) {
	r.calls++
	r.keys, r.principal, r.revoked = keys, principal, revoked
	return true, nil
}

func revokedIDs(count int) []string {
	ids := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("paperboat:%06d:machine_%s", i, strings.Repeat("x", 30))
	}
	return ids
}

func newRevokedTransferServer(t *testing.T, now *time.Time) (*Server, *recordingReconciler) {
	t.Helper()
	reconciler := &recordingReconciler{}
	server, err := New(Config{SocketPath: `\\.\pipe\PaperboatRevokedTransferTest`, StatePath: filepath.Join(t.TempDir(), "availability.json"), Applier: windowsTestApplier{}, Version: "test", AuthorizedKeys: reconciler, Now: func() time.Time { return *now }})
	if err != nil {
		t.Fatal(err)
	}
	return server, reconciler
}

func runTransfer(t *testing.T, server *Server, steps []Request) []Response {
	t.Helper()
	responses := make([]Response, len(steps))
	for i, step := range steps {
		responses[i] = windowsPipeRequest(t, server, context.Background(), step)
	}
	return responses
}

const testTransferID = "00112233445566778899aabbccddeeff"

// The revocation list is far over the 64 KiB request cap; it still arrives whole, applied once, only at the commit.
func TestWindowsRevokedKeyListAboveTheRequestCapIsAppliedAtomicallyAtTheCommit(t *testing.T) {
	now := time.Unix(1000, 0)
	server, reconciler := newRevokedTransferServer(t, &now)
	revoked := revokedIDs(2000)
	steps, err := revokedTransferSteps(testTransferID, []string{"ssh-ed25519 AAAA"}, "pb:machine_1", revoked)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if body, _ := json.Marshal(step); len(body)+1 > windowsHostServiceMaxRequestSize {
			t.Fatalf("a transfer step is %d bytes, over the request cap", len(body))
		}
	}
	responses := runTransfer(t, server, steps[:len(steps)-1])
	for i, response := range responses {
		if response.ErrorCode != "" {
			t.Fatalf("step %d refused: %s", i, response.ErrorCode)
		}
	}
	if reconciler.calls != 0 {
		t.Fatal("the list was applied before the commit")
	}
	commit := windowsPipeRequest(t, server, context.Background(), steps[len(steps)-1])
	if commit.ErrorCode != "" || !commit.AuthorizedKeysReconciled || !commit.AuthorizedKeysChanged {
		t.Fatalf("commit = %+v", commit)
	}
	if reconciler.calls != 1 || reconciler.principal != "pb:machine_1" || !reflect.DeepEqual(reconciler.revoked, revoked) || !reflect.DeepEqual(reconciler.keys, []string{"ssh-ed25519 AAAA"}) {
		t.Fatalf("reconciler saw %d calls, %d ids", reconciler.calls, len(reconciler.revoked))
	}
	if again := windowsPipeRequest(t, server, context.Background(), steps[len(steps)-1]); again.ErrorCode != "invalid_request" || reconciler.calls != 1 {
		t.Fatalf("a committed transfer was replayed: %+v", again)
	}
}

// An incomplete, tampered, expired or out-of-order transfer applies nothing.
func TestWindowsRevokedTransferThatIsNotWholeAppliesNothing(t *testing.T) {
	for name, tc := range map[string]func(steps []Request, now *time.Time) []Request{
		"missing chunk": func(steps []Request, _ *time.Time) []Request { return append(steps[:2:2], steps[len(steps)-1]) },
		"wrong count":   func(steps []Request, _ *time.Time) []Request { steps[0].TotalCount++; return steps },
		"wrong digest": func(steps []Request, _ *time.Time) []Request {
			steps[0].TotalDigest = strings.Repeat("0", 64)
			return steps
		},
		"changed id": func(steps []Request, _ *time.Time) []Request {
			steps[1].SSHRevoked[0] = "paperboat:other"
			return steps
		},
		"out of order": func(steps []Request, _ *time.Time) []Request { steps[2].ChunkIndex = 5; return steps },
		"unknown id": func(steps []Request, _ *time.Time) []Request {
			steps[len(steps)-1].TransferID = strings.Repeat("a", 32)
			return steps
		},
		"invalid id": func(steps []Request, _ *time.Time) []Request { steps[1].SSHRevoked[0] = "not-paperboat"; return steps },
		"expired": func(steps []Request, now *time.Time) []Request {
			*now = now.Add(revokedTransferTTL + time.Second)
			return steps
		},
		"too many in one chunk": func(steps []Request, _ *time.Time) []Request {
			steps[1].SSHRevoked = revokedIDs(revokedChunkMaxIDs + 1)
			return steps
		},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Unix(1000, 0)
			server, reconciler := newRevokedTransferServer(t, &now)
			steps, err := revokedTransferSteps(testTransferID, nil, "pb:machine_1", revokedIDs(600))
			if err != nil {
				t.Fatal(err)
			}
			steps = tc(steps, &now)
			responses := make([]Response, 0, len(steps))
			for i, step := range steps {
				if name == "expired" && i == len(steps)-1 {
					now = now.Add(revokedTransferTTL + time.Second)
				}
				responses = append(responses, windowsPipeRequest(t, server, context.Background(), step))
			}
			if last := responses[len(responses)-1]; last.AuthorizedKeysReconciled || last.ErrorCode == "" {
				t.Fatalf("the last step was accepted: %+v", last)
			}
			if reconciler.calls != 0 {
				t.Fatalf("an inconsistent transfer was applied (%d calls)", reconciler.calls)
			}
		})
	}
}

func TestWindowsRevokedTransferBeginReplacesTheTransferBeforeIt(t *testing.T) {
	now := time.Unix(1000, 0)
	server, reconciler := newRevokedTransferServer(t, &now)
	first, _ := revokedTransferSteps(testTransferID, nil, "first", revokedIDs(10))
	second, _ := revokedTransferSteps(hex.EncodeToString([]byte("0123456789abcdef")), nil, "second", revokedIDs(20))
	runTransfer(t, server, first[:2])
	runTransfer(t, server, second[:len(second)-1])
	if stale := windowsPipeRequest(t, server, context.Background(), first[len(first)-1]); stale.ErrorCode != "invalid_request" {
		t.Fatalf("the replaced transfer committed: %+v", stale)
	}
	if done := windowsPipeRequest(t, server, context.Background(), second[len(second)-1]); done.ErrorCode != "" || reconciler.calls != 1 || reconciler.principal != "second" || len(reconciler.revoked) != 20 {
		t.Fatalf("the live transfer did not commit: %+v calls=%d", done, reconciler.calls)
	}
}

func TestWindowsRevokedTransferStepsStayWithinTheRequestCapForTheLongestIDs(t *testing.T) {
	longest := "paperboat:" + strings.Repeat("k", 512-len("paperboat:"))
	revoked := make([]string, 1000)
	for i := range revoked {
		revoked[i] = longest
	}
	steps, err := revokedTransferSteps(testTransferID, nil, "pb:machine_1", revoked)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, step := range steps {
		body, _ := json.Marshal(step)
		if len(body)+1 > windowsHostServiceMaxRequestSize || len(step.SSHRevoked) > revokedChunkMaxIDs {
			t.Fatalf("step %s is %d bytes with %d ids", step.Operation, len(body), len(step.SSHRevoked))
		}
		total += len(step.SSHRevoked)
	}
	if total != len(revoked) {
		t.Fatalf("steps carry %d of %d ids", total, len(revoked))
	}
}

// The client picks the transfer by itself when one request cannot carry the list, over the real SID-restricted pipe.
func TestWindowsClientSendsAnOversizedRevocationListAsATransfer(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		t.Fatalf("resolve current SID: %v", err)
	}
	pipe := `\\.\pipe\PaperboatHostServiceRevokedTest-` + strconv.FormatInt(time.Now().UnixNano(), 10)
	ready := make(chan struct{})
	reconciler := &recordingReconciler{}
	server, err := New(Config{SocketPath: pipe, StatePath: filepath.Join(t.TempDir(), "availability.json"), SID: user.User.Sid.String(), Applier: windowsTestApplier{}, Version: "test", AuthorizedKeys: reconciler, Ready: func() error { close(ready); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("host service did not become ready")
	}
	client, err := NewClient(pipe, 10*time.Second)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	revoked := revokedIDs(5000)
	changed, err := client.ReconcileAuthorizedKeys(context.Background(), []string{"ssh-ed25519 AAAA"}, "pb:machine_1", revoked)
	if err != nil || !changed || reconciler.calls != 1 || !reflect.DeepEqual(reconciler.revoked, revoked) {
		cancel()
		t.Fatalf("changed=%t calls=%d ids=%d err=%v", changed, reconciler.calls, len(reconciler.revoked), err)
	}
	small := revokedIDs(3)
	if _, err := client.ReconcileAuthorizedKeys(context.Background(), nil, "pb:machine_1", small); err != nil || reconciler.calls != 2 || !reflect.DeepEqual(reconciler.revoked, small) {
		cancel()
		t.Fatalf("a small list did not take the single request: calls=%d err=%v", reconciler.calls, err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("host service shutdown error: %v", err)
	}
}
