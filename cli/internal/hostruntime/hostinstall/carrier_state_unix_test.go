//go:build linux || darwin

package hostinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"os"
	"path/filepath"
	"testing"
)

// Periodic reconciliation runs inside updated itself. An unrelated native
// service or install marker must not be stopped/rewritten during carrier-only
// rollback when the protected installed source stayed unchanged.
func TestCarrierOnlyRollbackPreservesInstalledRuntime(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected installation metadata requires root")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := installsource.Inspect(executable, "qualification", installsource.Custom)
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	request := generationRequest("a")
	request.UID, request.GID, request.Source = 2147483000, 2147483000, source
	// Real native names: the canonical carrier generation is derived from them.
	paths := platformPaths(request.UID)
	paths.worker, paths.workerNext, paths.metadata, paths.journal, paths.updateState = filepath.Join(root, "pb"), filepath.Join(root, "next"), filepath.Join(root, "metadata"), filepath.Join(root, "install-journal"), filepath.Join(root, "update-state")
	if err := os.WriteFile(paths.worker, image, 0755); err != nil {
		t.Fatal(err)
	}
	metadata := []byte(`{"protected":"unchanged installed identity"}`)
	if err := os.WriteFile(paths.metadata, metadata, 0600); err != nil {
		t.Fatal(err)
	}
	marker := []byte("existing activation marker")
	if err := os.WriteFile(paths.journal, marker, 0600); err != nil {
		t.Fatal(err)
	}
	baseline := unixCarrierBaseline{CarrierOnly: true, Worker: &source, Metadata: metadata, Candidate: source}
	body, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	state := unixCarrierInstallation{request: request, paths: paths}
	if err := state.Restore(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	if err := source.Verify(paths.worker); err != nil {
		t.Fatal("installed runtime changed", err)
	}
	for path, want := range map[string][]byte{paths.metadata: metadata, paths.journal: marker} {
		actual, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(actual, want) {
			t.Fatalf("carrier rollback changed installed state: %v", err)
		}
	}
}

// The restarted baseline carrier decides serving or standby from the active descriptor when it starts.
func TestRestoreBaselineCarrierPublishesItsDescriptorBeforeStartingIt(t *testing.T) {
	var steps []string
	publish := func(CarrierConfig) error { steps = append(steps, "publish"); return nil }
	start := func(context.Context, CarrierConfig) error { steps = append(steps, "start"); return nil }
	if err := restoreBaselineCarrier(context.Background(), CarrierConfig{}, publish, start); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || steps[0] != "publish" || steps[1] != "start" {
		t.Fatalf("steps %v: the baseline must be active before it starts", steps)
	}
	steps = nil
	failed := func(CarrierConfig) error { return os.ErrPermission }
	if err := restoreBaselineCarrier(context.Background(), CarrierConfig{}, failed, start); err == nil || len(steps) != 0 {
		t.Fatalf("a baseline that could not be made active was started: %v %v", steps, err)
	}
}

// pbvm1 F14-03: a killed install's lifecycle journal records the hostd of its CANDIDATE generation, but a recovery
// builds its services from the installed record (the previous generation). Restoring "not installed" through that
// unit stopped and removed the serving hostd, and nothing started it again: the machine went offline with no hostd.
// The recovery must address the journaled candidate's unit.
func TestRecoveryAddressesTheJournaledCandidateHostdNotTheServingOne(t *testing.T) {
	serving, candidate := generationRequest("a"), generationRequest("b")
	captured, err := json.Marshal(unixCarrierBaseline{Worker: &serving.Source, Candidate: candidate.Source})
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := journaledCandidateRequest(serving, captured) // the request a recovery starts from
	if err != nil {
		t.Fatal(err)
	}
	hostd, _, err := installersWithMaintenance(recovery, platformPaths(serving.UID), true, false)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := sourceGeneration(candidate.Source)
	if err != nil {
		t.Fatal(err)
	}
	if want := hostdGenerationDefinition(serving.Platform, serving.UID, generation); hostd.DefinitionPath() != want {
		t.Fatalf("recovery addresses %s, want the candidate's %s", hostd.DefinitionPath(), want)
	}
	if _, err := journaledCandidateRequest(serving, []byte(`{"candidate":{}}`)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a baseline without a valid candidate was accepted: %v", err)
	}
}
