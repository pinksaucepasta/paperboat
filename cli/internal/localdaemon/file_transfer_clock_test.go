package localdaemon

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

// TestFileTransferBrokerJudgesTheDeadlineOnTheServerClock: on a machine whose clock is 10 minutes ahead,
// a server-issued transfer deadline 2 minutes away is still valid (F21-02; pb send failed on a skewed Mac).
func TestFileTransferBrokerJudgesTheDeadlineOnTheServerClock(t *testing.T) {
	serverclock.Observe(time.Now().UTC().Add(-10*time.Minute).Format(http.TimeFormat), time.Now(), time.Now())
	t.Cleanup(func() { serverclock.Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now()) })
	broker, err := NewFileTransferBroker(transferPreparerFake{direct: &transferDirectFake{}, lifetime: make(chan context.Context, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	request := localapi.FileTransferRequest{Schema: localapi.FileTransferSchemaV1, MachineID: "machine_1", EnvironmentID: "environment_1", MachineGeneration: 1, OperationID: "operation_1", Credential: "credential_1", AccessSessionID: "access_1", Deadline: serverclock.Now().Add(2 * time.Minute), MaximumBytes: 1 << 20}
	result, err := broker.PrepareFileTransfer(context.Background(), localapi.Peer{UID: 1000, GID: 1000, PID: 41}, request)
	if err != nil || result.Handle == "" {
		t.Fatalf("a deadline valid on the server clock was refused on a machine 10 minutes ahead: result=%+v err=%v", result, err)
	}
}
