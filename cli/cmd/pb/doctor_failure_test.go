package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestLocalDoctorFilesystemFailureIsUnavailableAndObservedThenRecovers(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "PRIVATE_DOCTOR_PARENT")
	if err := os.WriteFile(parent, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "state")
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", root)
	ctx := supportref.WithContext(t.Context(), supportref.New())
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults = append(faults, fault) })
	t.Cleanup(restore)
	report := collectLocalDoctor(ctx)
	var original *os.PathError
	if report.SetupState != "unavailable" || report.IdentityState != "unavailable" || !errors.As(report.failure, &original) {
		t.Fatal("filesystem failure became missing or invalid identity")
	}
	if len(faults) != 1 || faults[0].SupportReference != supportref.FromContext(ctx) || faults[0].Cause == "unknown" {
		t.Fatal("doctor consumed original correlated filesystem evidence")
	}
	if strings.Contains(strings.Join(report.RecoveryActions, " "), "revoke") {
		t.Fatal("temporary access failure suggested destroying machine identity")
	}
	// Payload fields intentionally include the user-selected state root; the
	// original error itself must never become another diagnostic JSON field.
	report.StateRoot = ""
	encoded, err := json.Marshal(report)
	if err != nil || strings.Contains(string(encoded), "PRIVATE_DOCTOR_PARENT") {
		t.Fatal("doctor serialized its original private error")
	}
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	recovered := collectLocalDoctor(ctx)
	if recovered.SetupState != "not_set_up" || recovered.failure != nil || len(faults) != 1 {
		t.Fatal("doctor did not recover after filesystem repair")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("doctor created an unconfigured installation")
	}
}
