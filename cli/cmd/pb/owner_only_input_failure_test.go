package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/environmentmanager"
)

func TestUserInputFileRejectionRequiresPureExpectedCause(t *testing.T) {
	for _, cause := range []error{os.ErrNotExist, os.ErrPermission, errOwnerOnlyFileInvalid} {
		pathCause := &os.PathError{Op: "open", Path: "/PRIVATE_INPUT_PATH", Err: cause}
		err := userInputFileError("Check the selected input file.", pathCause)
		if !errors.Is(err, cause) || classifyCommandFailure(err).kind != commandUsage {
			t.Fatal("expected selected-file rejection lost cause or usage classification")
		}
		result := classifyCLIJSONError(err)
		if result.Code != "invalid_invocation" || result.StateChanged != false || strings.Contains(result.Message, "PRIVATE") {
			t.Fatal("selected-file rejection lost public invocation contract")
		}
		mixed := userInputFileError("Check the selected input file.", errors.Join(pathCause, syscall.EIO))
		if !errors.Is(mixed, cause) || !errors.Is(mixed, syscall.EIO) || classifyCommandFailure(mixed).kind != commandUnexpected || classifyCLIJSONError(mixed).StateChanged != "unknown" {
			t.Fatal("selected-file rejection hid an independent I/O failure")
		}
	}
	if classifyCommandFailure(userInputFileError("Check the selected input file.", cyclicEnvironmentCause{})).kind != commandUnexpected {
		t.Fatal("cyclic input failure became an invocation rejection")
	}
}

func TestENVRecipientChangedStateRequiresAffirmativeProof(t *testing.T) {
	for _, fixture := range []struct {
		name string
		err  *envHostRefreshFailure
		want any
	}{
		{"rotation response lost", &envHostRefreshFailure{cause: &environmentmanager.LayerPublicationPending{Cause: syscall.EIO}}, "unknown"},
		{"mixed failure without proof", &envHostRefreshFailure{cause: errors.Join(environmentmanager.ErrVaultLocked, syscall.EIO)}, "unknown"},
		{"source saved before failure", &envHostRefreshFailure{sourceChanged: true, cause: syscall.EIO}, true},
		{"vault completed before failure", &envHostRefreshFailure{operationCompleted: true, cause: syscall.EIO}, true},
		{"earlier recipient completed", &envHostRefreshFailure{completed: 1, cause: syscall.EIO}, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			result := classifyCLIJSONError(fixture.err)
			if result.StateChanged != fixture.want || !result.OutcomeUncertain {
				t.Fatal("missing mutation proof became unchanged state or known progress was lost")
			}
		})
	}
}

func TestOwnerOnlyFileSeparatesValidationFromFilesystemFailure(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "PRIVATE_MISSING_INPUT")
	data, err := readOwnerOnlyFile(missing, 8)
	if data != nil || !errors.Is(err, os.ErrNotExist) || errors.Is(err, errOwnerOnlyFileInvalid) {
		t.Fatal("missing file was mislabeled as local metadata validation")
	}
	// This low-level reader is also used for internal tokens. Its missing-file
	// error must remain operational unless an explicit input caller owns it.
	if classifyCommandFailure(err).kind != commandUnexpected {
		t.Fatal("internal missing file became a user invocation rejection")
	}
	path := filepath.Join(root, "owner-only-input")
	file, err := createEnvironmentRecoveryFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString("PRIVATE_VALUE")
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal("create owner-only input fixture failed")
	}
	data, err = readOwnerOnlyFile(path, 8)
	if data != nil || !errors.Is(err, errOwnerOnlyFileInvalid) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("oversized owner-only input was exposed or lost validation reason")
	}
	data, err = readOwnerOnlyFile(path, 32)
	if err != nil || string(data) != "PRIVATE_VALUE" {
		t.Fatal("valid owner-only input did not recover")
	}
	clear(data)
}
