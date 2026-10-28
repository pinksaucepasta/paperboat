package main

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"

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

// The server's Automatic policy sends max_file_bytes 0 with automatic_max_batch_bytes; a lease sized from
// MaxFileBytes asked the daemon for 0 bytes, so every pb connect printed "File transfer is unavailable".
func TestConnectFileTransferLeaseFitsTheAutomaticPolicy(t *testing.T) {
	policy := api.FileTransferPolicy{AutomaticMaxBatchBytes: api.AutomaticTransferLimit}
	request := fileTransferLeaseRequest("machine_1", "env_1", 1, "operation_1", "token", "session_1", time.Now().Add(2*time.Minute), policy, api.TransferDeclaration{})
	if err := request.Validate(time.Now()); err != nil || request.MaximumBytes < uint64(api.AutomaticTransferLimit) {
		t.Fatalf("lease bytes=%d err=%v", request.MaximumBytes, err)
	}
}

// An Unlimited send of the full 1 TiB must still fit the daemon's lease limit once framing room is added.
func TestUnlimitedFileTransferLeaseStaysWithinTheDaemonLimit(t *testing.T) {
	request := fileTransferLeaseRequest("machine_1", "env_1", 1, "operation_1", "token", "session_1", time.Now().Add(2*time.Minute), api.FileTransferPolicy{}, api.TransferDeclaration{Mode: "unlimited", Bytes: 1 << 40})
	if err := request.Validate(time.Now()); err != nil {
		t.Fatalf("lease bytes=%d err=%v", request.MaximumBytes, err)
	}
}
