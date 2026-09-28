package runtimeattachment

import "testing"

func TestNewUsesOneSharedCarrierWithBrowserStreamCapacity(t *testing.T) {
	service, err := New(Config{
		ControlURL: "https://api.example.test", StateRoot: t.TempDir(), DeviceID: "device-test",
		WorkerGeneration: 1, InstallationGeneration: 1, ListenAddress: "127.0.0.1:38080",
	})
	if err != nil {
		t.Fatal(err)
	}
	if service.sessions == nil {
		t.Fatal("runtime attachment has no shared carrier source")
	}
}

func TestChooseLiveKeepsRecoveredPrimaryAsBackup(t *testing.T) {
	primary := admissionBinding{Binding: binding{EdgeNodeID: "edge-primary", EdgeProcessEpoch: "epoch-primary", SessionID: "session-primary"}}
	backup := admissionBinding{Binding: binding{EdgeNodeID: "edge-backup", EdgeProcessEpoch: "epoch-backup", SessionID: "session-backup"}}
	primaryLive, backupLive := &live{}, &live{}
	ordered := []admissionBinding{primary, backup}
	lives := map[string]*live{carrierKey(primary): primaryLive, carrierKey(backup): backupLive}
	chosen, admission := chooseLive(ordered, lives, backup.Binding.EdgeNodeID, backup.Binding.EdgeProcessEpoch)
	if chosen != backupLive || admission.Binding.EdgeNodeID != backup.Binding.EdgeNodeID {
		t.Fatal("recovered primary preempted healthy active backup")
	}
	delete(lives, carrierKey(backup))
	chosen, admission = chooseLive(ordered, lives, backup.Binding.EdgeNodeID, backup.Binding.EdgeProcessEpoch)
	if chosen != primaryLive || admission.Binding.EdgeNodeID != primary.Binding.EdgeNodeID {
		t.Fatal("failed active backup did not promote ready primary")
	}
	backup.Binding.EdgeProcessEpoch = "replacement-epoch"
	ordered[1] = backup
	lives[carrierKey(backup)] = backupLive
	chosen, admission = chooseLive(ordered, lives, "edge-backup", "epoch-backup")
	if chosen != primaryLive || admission.Binding.EdgeNodeID != primary.Binding.EdgeNodeID {
		t.Fatal("stale active edge epoch retained authority")
	}
}
