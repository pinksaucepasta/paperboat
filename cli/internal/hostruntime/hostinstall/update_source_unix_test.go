//go:build darwin || linux

package hostinstall

import (
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"os"
	"testing"
)

func TestUpdatedSourcePreservesEnrollmentAndRejectsWrongPayload(t *testing.T) {
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := installsource.Inspect(path, "2026.09.30.18", installsource.Official)
	if err != nil {
		t.Fatal(err)
	}
	prior := Request{UserMachineID: "machine", StateRoot: "/protected/state", InstallationGeneration: 9, Source: installsource.Source{AutomaticUpdates: false}}
	next, err := updatedRequestSource(prior, path, source)
	if err != nil {
		t.Fatal(err)
	}
	if next.Source.Version != source.Version || next.Artifact.Version != source.Version || next.Source.AutomaticUpdates || next.UserMachineID != prior.UserMachineID || next.StateRoot != prior.StateRoot || next.InstallationGeneration != prior.InstallationGeneration {
		t.Fatal("installation identity or preference changed")
	}
	source.SHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err = updatedRequestSource(prior, path, source); err == nil {
		t.Fatal("wrong committed payload accepted")
	}
}
