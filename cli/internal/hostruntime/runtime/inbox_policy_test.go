//go:build darwin || linux || windows

package runtime

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

func TestStaticPolicyBindsInboxCredentialsToTheirOperationAndSingleUse(t *testing.T) {
	resolver := staticPolicyResolver{issuer: "https://api.example.test", environmentID: "env_1", machineID: "machine_1"}
	for capability, want := range map[string][2]string{"inbox.read.v1": {"inbox_read", "inbox:read"}, "inbox.delete.v1": {"inbox_delete", "inbox:delete"}} {
		policy, err := resolver.Policy(protocol.Frame{Type: "request", Capability: capability, OperationID: "operation_inbox_1"})
		if err != nil {
			t.Fatal(err)
		}
		if policy.CredentialClass != want[0] || len(policy.Scopes) != 1 || policy.Scopes[0] != want[1] || policy.OperationID != "operation_inbox_1" || !policy.SingleUse || policy.MachineID != "machine_1" {
			t.Errorf("%s policy = %+v", capability, policy)
		}
	}
	controller := &machineCapabilityController{desired: machineCapabilitySelection{FileReceive: true}}
	if !controller.Enabled("inbox.read.v1") || !controller.Enabled("inbox.delete.v1") {
		t.Error("inbox capabilities follow file receive")
	}
	controller.desired.FileReceive = false
	if controller.Enabled("inbox.read.v1") || controller.Enabled("inbox.delete.v1") {
		t.Error("inbox capabilities must stop when file receive is off")
	}
}
