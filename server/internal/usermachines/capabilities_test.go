package usermachines

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

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
