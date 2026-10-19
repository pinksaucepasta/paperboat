package main

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

// Connecting a computer to itself has no file transfer to set up; it must not be reported as a failure.
func TestNativeFileTransferClientSkipsConnectingToSameMachine(t *testing.T) {
	target := resolver.ConnectInfo{
		TargetKind: "machine", MachineID: "machine_1", MachineGeneration: 1,
		Terminal: &resolver.TerminalTarget{EnvironmentID: "env_1"},
		FileTransfer: &resolver.FileTransferTarget{
			SourceMachineID: "machine_1", DestinationMachineID: "machine_1",
			Auth: resolver.AuthTarget{ResourceID: "session_1"},
		},
	}
	client, lease, err := nativeFileTransferClient(t.Context(), &localapi.Client{}, target, "operation_1")
	if client != nil || lease != nil || err != nil {
		t.Fatalf("client=%v lease=%v err=%v", client, lease, err)
	}
}
