//go:build darwin || linux

package updated

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

func TestCustomInstallationPreservesScheduleWithoutOfficialUpdateOperations(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("native updater storage requires root ownership")
	}
	root, err := os.MkdirTemp("", "pb-custom-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "pb")
	if err := os.WriteFile(binary, body, 0700); err != nil {
		t.Fatal(err)
	}
	source, err := installsource.Inspect(binary, "dev-custom", installsource.Custom)
	if err != nil {
		t.Fatal(err)
	}
	active, err := workerupdate.InstalledBaseline(source, binary)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{StateRoot: root, Binary: binary, BinaryRollback: filepath.Join(root, "previous"), BinaryStaged: filepath.Join(root, "next"), Active: active, WorkerUID: 0, WorkerGID: 0, SocketPath: filepath.Join(root, "hostd.sock"), Token: make([]byte, 32), RepositoryURL: "https://127.0.0.1:1/tuf", MachineID: "machine_test", Health: HTTPHealth{Endpoint: "http://127.0.0.1:1/healthz"}, ActivationGate: &gateFixture{}, ControlSocket: filepath.Join(root, "control.sock"), ActivationController: &controllerFixture{}, Participants: &participantFixture{}})
	if err != nil {
		t.Fatal(err)
	}
	settings := autoupdate.DefaultPreferences(true)
	settings.LocalTime = "04:30"
	if _, err := machineUpdateSettings(root, true, &settings); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := s.automaticCheck(ctx)
	if err != nil || result.Updated || result.Version != source.Version {
		t.Fatalf("custom scheduler attempted an official update: %+v %v", result, err)
	}
	for _, operation := range []string{"check", "download", "install"} {
		if _, err := s.controlRequestWithRequest(ctx, ControlRequest{Schema: ControlProtocolV1, Operation: operation}); !errors.Is(err, ErrCustomInstallation) {
			t.Fatalf("custom %s was not refused: %v", operation, err)
		}
	}
	if _, err := s.controlRequestWithRequest(ctx, ControlRequest{Schema: ControlProtocolV1, Operation: "settings", Settings: &settings}); !errors.Is(err, ErrCustomInstallation) {
		t.Fatalf("custom build enabled automatic updates: %v", err)
	}
	response, err := s.controlRequestWithRequest(ctx, ControlRequest{Schema: ControlProtocolV1, Operation: "settings"})
	if err != nil || response.Settings == nil || response.Settings.Enabled || response.Settings.LocalTime != "04:30" || !response.NextMaintenanceAt.IsZero() {
		t.Fatalf("custom settings report enabled updates: %+v %v", response, err)
	}
	stored, err := machineUpdateSettings(root, true, nil)
	if err != nil || stored != settings {
		t.Fatal("custom installation overwrote the official schedule", err)
	}
}
