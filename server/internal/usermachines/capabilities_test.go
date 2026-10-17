package usermachines

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
)

func TestMachineCapabilityNamesAreUnified(t *testing.T) {
	defaults := MachineCapabilitySelection{Terminal: true, ManagedSSH: true, FileReceive: true, PreviewTunnel: true}
	want := []string{"file_receive", "preview_launch", "terminal_host", "session_host", "ssh_host"}
	if got := capabilityNames(defaults); !reflect.DeepEqual(got, want) {
		t.Fatalf("default names=%q want=%q", got, want)
	}
}

func TestMachineCapabilityPolicyOmitsRetiredDeviceRelay(t *testing.T) {
	machine := dbsqlc.UserMachine{ConfiguredCapabilities: []string{"peer_relay"}}
	encoded, err := json.Marshal(mapMachineCapabilityPolicy(machine))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "peer_relay") {
		t.Fatalf("retired device relay remains in machine capability response: %s", encoded)
	}
}

func TestMapMachineCapabilityPolicySeparatesDesiredAndApplied(t *testing.T) {
	machine := dbsqlc.UserMachine{
		ConfiguredCapabilities:     []string{"file_receive", "ssh_host", "peer_relay"},
		ObservedCapabilities:       []string{"file_receive"},
		CapabilitiesDesiredVersion: 4, CapabilitiesObservedVersion: 3,
		CapabilitiesStatus: "pending", CapabilitiesErrorCode: sql.NullString{},
	}
	got := mapMachineCapabilityPolicy(machine)
	if got.Schema != MachineCapabilitiesSchemaV1 || got.DesiredVersion != 4 || got.AppliedVersion != 3 || !got.Desired.ManagedSSH || got.Desired.Terminal || got.Applied.ManagedSSH || got.Status != "pending" {
		t.Fatalf("policy=%+v", got)
	}
}

func TestMachineCapabilityMutationRejectsInvalidAuthorityBeforeStorage(t *testing.T) {
	service := &Service{}
	for _, input := range []struct {
		user, machine, key string
		version            int64
	}{
		{"", "mch_1", "capability-operation", 1},
		{"usr_1", "", "capability-operation", 1},
		{"usr_1", "mch_1", "short", 1},
		{"usr_1", "mch_1", "capability-operation", 0},
	} {
		if _, err := service.SetMachineCapabilities(context.Background(), input.user, input.machine, input.key, MachineCapabilitySelection{}, input.version); !errors.Is(err, ErrCapabilitiesInvalid) {
			t.Fatalf("input=%+v error=%v", input, err)
		}
	}
}

func TestMachineCapabilityApplicationMatchesDesiredPostgres(t *testing.T) {
	f := newWorkspaceMachineFixture(t)
	environmentID := "env_" + f.suffix
	initial, err := f.service.ResolveMachineCapabilities(f.ctx, f.machine, environmentID)
	if err != nil {
		t.Fatal(err)
	}
	desired := MachineCapabilitySelection{ManagedSSH: true}
	policy, err := f.service.SetMachineCapabilities(f.ctx, f.account, f.machine, "capability-observation-"+f.suffix, desired, initial.DesiredVersion)
	if err != nil {
		t.Fatal(err)
	}
	observed := MachineCapabilitiesObservation{Schema: MachineCapabilitiesSchemaV1, Version: policy.DesiredVersion, Applied: initial.Desired, Status: "applied", ObservedAt: time.Now().UTC()}
	if err := f.service.RecordMachineCapabilitiesObservation(f.ctx, f.machine, environmentID, observed); !errors.Is(err, ErrCapabilitiesObservationStale) {
		t.Fatalf("different desired capabilities reported applied: %v", err)
	}
	unchanged, err := f.service.ResolveMachineCapabilities(f.ctx, f.machine, environmentID)
	if err != nil || unchanged.AppliedVersion != initial.AppliedVersion || unchanged.Applied != initial.Applied || unchanged.Status != "pending" {
		t.Fatal("rejected capability acknowledgement changed observed state")
	}
	observed.Status, observed.ErrorCode = "error", "apply_failed"
	if err := f.service.RecordMachineCapabilitiesObservation(f.ctx, f.machine, environmentID, observed); err != nil {
		t.Fatalf("actual previous capabilities on application failure: %v", err)
	}
	observed.Status, observed.ErrorCode, observed.Applied = "applied", "", desired
	if err := f.service.RecordMachineCapabilitiesObservation(f.ctx, f.machine, environmentID, observed); err != nil {
		t.Fatalf("successful application after failure: %v", err)
	}
	final, err := f.service.ResolveMachineCapabilities(f.ctx, f.machine, environmentID)
	if err != nil || final.Applied != desired || final.AppliedVersion != policy.DesiredVersion || final.Status != "applied" {
		t.Fatal("successful acknowledgement did not publish exact desired state")
	}
	// A machine with every incoming service disabled still acknowledges its
	// policy. An empty selection must be stored as an empty array, not SQL NULL.
	policy, err = f.service.SetMachineCapabilities(f.ctx, f.account, f.machine, "capability-disable-all-"+f.suffix, MachineCapabilitySelection{}, final.DesiredVersion)
	if err != nil {
		t.Fatalf("disable all capabilities: %v", err)
	}
	observed.Version, observed.Applied = policy.DesiredVersion, MachineCapabilitySelection{}
	if err := f.service.RecordMachineCapabilitiesObservation(f.ctx, f.machine, environmentID, observed); err != nil {
		t.Fatalf("acknowledge all capabilities disabled: %v", err)
	}
	final, err = f.service.ResolveMachineCapabilities(f.ctx, f.machine, environmentID)
	if err != nil || final.Desired != (MachineCapabilitySelection{}) || final.Applied != final.Desired || final.AppliedVersion != policy.DesiredVersion || final.Status != "applied" {
		t.Fatal("all-disabled acknowledgement did not publish exact desired state")
	}
	if err := f.service.RecordMachineCapabilitiesObservation(f.ctx, f.machine, environmentID+"-foreign", observed); !errors.Is(err, ErrCapabilitiesObservationStale) {
		t.Fatalf("foreign environment changed capabilities: %v", err)
	}
	if _, err := f.store.SQL().ExecContext(f.ctx, `UPDATE paperboat.user_machines SET state='revoked' WHERE id=$1`, f.machine); err != nil {
		t.Fatal(err)
	}
	if err := f.service.RecordMachineCapabilitiesObservation(f.ctx, f.machine, environmentID, observed); !errors.Is(err, ErrCapabilitiesObservationStale) {
		t.Fatalf("revoked machine published applied capabilities: %v", err)
	}
}
