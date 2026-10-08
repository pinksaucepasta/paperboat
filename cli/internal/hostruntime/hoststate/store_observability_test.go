package hoststate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
)

type cyclicStoreCause struct{}

func (*cyclicStoreCause) Error() string       { return "cyclic cause" }
func (cause *cyclicStoreCause) Unwrap() error { return cause }

type chainedStoreCause struct{ cause error }

func (cause chainedStoreCause) Error() string { return "chained cause" }
func (cause chainedStoreCause) Unwrap() error { return cause.cause }

type nilChildStoreCause struct{}

func (nilChildStoreCause) Error() string   { return "invalid nil child" }
func (nilChildStoreCause) Unwrap() []error { return []error{os.ErrNotExist, nil} }

func TestPureNotExistRequiresCompleteBoundedMissingLeaves(t *testing.T) {
	// os.IsNotExist uses the platform's native missing-file classifications.
	missing := os.ErrNotExist
	file, nativeMissing := os.Open(filepath.Join(t.TempDir(), "missing-state-file"))
	if nativeMissing == nil {
		_ = file.Close()
		t.Fatal("temporary missing-file path unexpectedly exists")
	}
	var nativePathError *os.PathError
	if !errors.As(nativeMissing, &nativePathError) {
		t.Fatal("missing-file operation did not return an os.PathError")
	}
	var typedNil *cyclicStoreCause
	overBudget := error(os.ErrNotExist)
	for i := 0; i < maxAbsenceCauseNodes; i++ {
		overBudget = chainedStoreCause{cause: overBudget}
	}
	for name, err := range map[string]error{
		"bare missing":         missing,
		"wrapped missing":      fmt.Errorf("outer: %w", missing),
		"safe wrapper missing": safeStoreFailure("safe", missing),
		"native path missing":  nativeMissing,
	} {
		t.Run(name, func(t *testing.T) {
			if !pureNotExist(err) {
				t.Fatalf("pure missing cause was not recognized: %T", err)
			}
		})
	}

	for name, err := range map[string]error{
		"mixed I/O failure":     safeStoreFailure("safe", missing, errors.New("private close failure")),
		"mixed custody failure": safeStoreFailure("safe", missing, ErrInvalidState),
		"cycle":                 &cyclicStoreCause{},
		"nil child":             nilChildStoreCause{},
		"typed nil":             typedNil,
		"over-budget chain":     overBudget,
		"nil":                   nil,
	} {
		t.Run(name, func(t *testing.T) {
			if pureNotExist(err) {
				t.Fatalf("non-pure missing cause authorized absence: %T", err)
			}
		})
	}
}

func TestOpenInitializesOnGenuineMissingStateOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	store, status, err := Open(Config{Root: root})
	if err != nil {
		t.Fatalf("initialization after genuine missing candidates failed: %v", err)
	}
	defer store.Close()
	if status.Degraded || status.Code != "initialized" || status.Source != "initial" {
		t.Fatalf("genuine missing files did not initialize the store: %+v", status)
	}
}

func TestOpenFilesystemFailureRetainsCauseWithoutExposingPath(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "private-state-root")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(blocker, "nested-state")
	store, _, err := Open(Config{Root: root})
	if store != nil {
		_ = store.Close()
	}
	var pathErr *os.PathError
	if err == nil || !errors.As(err, &pathErr) || strings.Contains(err.Error(), blocker) || strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Open error does not safely retain its filesystem cause: %v", err)
	}
}

func TestCommitFailureRetainsCauseWithoutFormattingIt(t *testing.T) {
	privateCause := errors.New("PRIVATE_STATE_VALUE /private/state/root")
	armed := false
	store, _, err := Open(Config{
		Root: filepath.Join(t.TempDir(), "state"),
		FailureHook: func(phase Phase) error {
			if armed && phase == PhaseCommitStaged {
				return privateCause
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	armed = true
	_, err = store.Commit(1, validState(t, 1, 1))
	var commitErr *CommitError
	if err == nil || !errors.As(err, &commitErr) || !errors.Is(err, privateCause) || errors.Is(err, ErrUncertain) || strings.Contains(err.Error(), "PRIVATE_STATE_VALUE") || strings.Contains(err.Error(), "/private/state/root") {
		t.Fatalf("commit error lost its phase/cause contract or exposed private text: %v", err)
	}
	if commitErr.Changed || commitErr.Phase != PhaseCommitStaged {
		t.Fatalf("commit error changed=%v phase=%s", commitErr.Changed, commitErr.Phase)
	}
}

func TestSafeStorageErrorRetainsAtomicCommitOutcome(t *testing.T) {
	atomicErr := &atomicfile.Error{
		Stage: atomicfile.StageSyncDir,
		Path:  "/private/state.json",
		Err:   errors.New("PRIVATE_WRITE_FAILURE"),
	}
	wrapped := safeStoreFailure("host state file could not be written", atomicErr)
	var retained *atomicfile.Error
	if !atomicWriteMayHaveChanged(wrapped) || !errors.As(wrapped, &retained) || retained != atomicErr || strings.Contains(wrapped.Error(), "/private/state.json") || strings.Contains(wrapped.Error(), "PRIVATE_WRITE_FAILURE") {
		t.Fatalf("safe error changed atomic-write outcome or exposed private text: %v", wrapped)
	}
}

func TestDocumentCorruptionRetainsParserCauseWithoutExposingRecord(t *testing.T) {
	const privateRecordValue = "PRIVATE_HOST_STATE_RECORD"
	raw := []byte(`{"schema":"paperboat.host-state","schema_version":1,"private":"` + privateRecordValue + `","broken":@}`)
	_, _, err := decodeAnyDocument(raw)
	var syntaxErr *json.SyntaxError
	if err == nil || !errors.Is(err, ErrCorrupt) || !errors.As(err, &syntaxErr) || err.Error() != "host state document is invalid" || strings.Contains(err.Error(), privateRecordValue) {
		t.Fatalf("document error did not safely retain parser cause: %v", err)
	}
}
