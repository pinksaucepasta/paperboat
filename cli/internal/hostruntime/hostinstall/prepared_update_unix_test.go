//go:build linux || darwin

package hostinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

// Ordinary lifecycle recovery has no signed-policy authorizer. A protected
// handoff must fence it even before any participant has been touched, while
// absent updater state remains a valid uninstall inventory case.
func TestOrdinaryNativeRecoveryPreservesSignedHandoff(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires protected activation custody")
	}
	parent, err := os.MkdirTemp("/tmp", "paperboat-prepared-fence-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	parent, err = filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatal(err)
	}
	paths := installPaths{updateState: filepath.Join(parent, "updated")}
	if err = prepareUnixNativeRecovery(context.Background(), paths, 0, false); err != nil {
		t.Fatal("absent updater state blocked uninstall", err)
	}
	if err = os.Mkdir(paths.updateState, 0700); err != nil {
		t.Fatal(err)
	}
	if err = prepareUnixNativeRecovery(context.Background(), paths, 0, false); err != nil {
		t.Fatal("idle state blocked lifecycle", err)
	}
	if err = os.Mkdir(filepath.Join(paths.updateState, "activation"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(paths.updateState, "activation", "handoff.json")
	for _, started := range []bool{false, true} {
		body, err := json.Marshal(map[string]any{"schema": "paperboat.update-activation/v1", "previous": workerupdate.Release{Version: "previous", SHA256: strings.Repeat("a", 64), Length: 1}, "candidate": "candidate", "approval_id": strings.Repeat("b", 64), "started": started, "participants_touched": started})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		if err = prepareUnixNativeRecovery(context.Background(), paths, 0, false); !errors.Is(err, updated.ErrActivationPending) {
			t.Fatal("signed handoff reached ordinary recovery", err)
		}
		after, e := os.ReadFile(path)
		if e != nil || !bytes.Equal(after, body) {
			t.Fatal("signed recovery authority changed", e)
		}
	}
}

// The native receipt supplies its captured baseline to this account-binding
// validator; journal custody remains covered by service lifecycle tests. A pending
// native baseline authorizes recovery of the prior installation, not arbitrary
// candidate bytes; committed proof instead requires the exact candidate image.
func TestPreparedUpdateRecoveryProofBindsProtectedInstallation(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires protected native receipt custody")
	}
	for _, mode := range []string{"untouched", "native baseline", "committed candidate", "wrong committed payload", "wrong committed candidate", "wrong uid", "wrong baseline", "foreign baseline account", "malformed baseline", "unknown baseline field", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			request := validRequest(t)
			root, err := os.MkdirTemp(request.Home, "paperboat-recovery-proof-*")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			paths := installPaths{instance: "u0", root: root, environmentCredential: filepath.Join(root, "environment.cred"), worker: filepath.Join(root, "pb"), workerRollback: filepath.Join(root, "releases", "rollback"), workerNext: filepath.Join(root, "releases", "next"), metadata: filepath.Join(root, "installation.json"), journal: filepath.Join(root, "install-journal.json"), updateState: filepath.Join(root, "update-state"), hostdToken: filepath.Join(root, "hostd.token"), hostdSocket: filepath.Join(root, "hostd.sock"), updaterSocket: filepath.Join(root, "updater.sock")}
			image, err := os.ReadFile(request.Executable)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(paths.worker, image, 0755); err != nil {
				t.Fatal(err)
			}
			request.Executable = paths.worker
			previous := request.Source
			previous.Version = "prior"
			candidate := request.Source
			candidate.Version = "candidate"
			candidate.Distribution = installsource.Official
			request.Source = previous
			if mode == "committed candidate" || mode == "wrong committed payload" || mode == "wrong committed candidate" {
				request.Source = candidate
			}
			if err = writeInstallMetadata(paths.metadata, request); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			uid := request.UID
			if mode == "wrong uid" {
				uid++
			}
			if mode == "wrong committed candidate" {
				candidate.Version = "unapproved"
			}
			if mode == "wrong committed payload" {
				if err = os.WriteFile(paths.worker, []byte("tampered"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			native := mode != "untouched" && !strings.Contains(mode, "committed") && mode != "canceled"
			if native {
				metadata, err := os.ReadFile(paths.metadata)
				if err != nil {
					t.Fatal(err)
				}
				baseline := unixCarrierBaseline{Worker: &previous, Metadata: metadata, Candidate: candidate}
				if mode == "wrong baseline" {
					other := previous
					other.Version = "foreign"
					baseline.Worker = &other
				}
				if mode == "foreign baseline account" {
					foreign := request
					foreign.UID++
					baseline.Metadata, err = json.Marshal(foreign)
					if err != nil {
						t.Fatal(err)
					}
				}
				body, err := json.Marshal(baseline)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "malformed baseline" {
					body = []byte(`{"worker":false}`)
				}
				if mode == "unknown baseline field" {
					body = append(body[:len(body)-1], []byte(`,"foreign":true}`)...)
				}
				got, err := preparedUpdateBaselineMatches(body, uid, request, previous)
				if mode == "native baseline" {
					if err != nil || !got {
						t.Fatalf("native baseline identity rejected: %v", err)
					}
				} else if got || err == nil {
					t.Fatalf("foreign/malformed native baseline accepted: %v", err)
				}
				return

			}
			if mode == "canceled" {
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			}
			got, err := preparedUpdateRecoveryStarted(ctx, paths, uid, previous, candidate)
			expected := mode == "native baseline" || mode == "committed candidate"
			if expected {
				if err != nil || !got {
					t.Fatalf("protected recovery proof rejected: %v", err)
				}
			} else if got || (mode != "untouched" && mode != "wrong committed candidate" && err == nil) {
				t.Fatalf("unbound recovery proof accepted: started=%v error=%v", got, err)
			}
		})
	}
}

// The staged candidate is consumed when it becomes the worker. Pins for the
// new generation are copied after that point, so the request must name the
// installed worker; the staging path is gone (pbvm1 official update failed
// with lstat ENOENT on it, rolling every update back).
func TestPreparedCandidateActivationLeavesTheRequestPointingAtTheWorker(t *testing.T) {
	request := validRequest(t)
	image, err := os.ReadFile(request.Executable)
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	paths := installPaths{worker: filepath.Join(parent, "pb"), workerNext: filepath.Join(parent, "pb.staged")}
	if err = os.WriteFile(paths.workerNext, image, 0755); err != nil {
		t.Fatal(err)
	}
	request.Executable = paths.workerNext
	got, err := activatePreparedCandidate(request, paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(paths.workerNext); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the staged candidate was not consumed", err)
	}
	if got.Executable != paths.worker {
		t.Fatalf("request still names %q after the staged file was consumed", got.Executable)
	}
	if err = got.Source.Verify(got.Executable); err != nil {
		t.Fatal("later pin staging cannot verify the installed worker:", err)
	}
}
