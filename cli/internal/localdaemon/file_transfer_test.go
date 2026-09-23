package localdaemon

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

type transferPreparerFake struct {
	direct   *transferDirectFake
	lifetime chan context.Context
}

func (f transferPreparerFake) PrepareNativeFileTransfer(lifetime context.Context, _ resolver.ConnectInfo, _ string) (tunnel.DirectTransferStreamOpener, error) {
	if f.lifetime != nil {
		f.lifetime <- lifetime
	}
	return f.direct, nil
}

type transferDirectFake struct{ closed bool }

func (f *transferDirectFake) Close() error { f.closed = true; return nil }
func (*transferDirectFake) OpenTransferStream(context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	_ = server.Close()
	return client, nil
}

func TestFileTransferBrokerBindsHandleToUnixPeer(t *testing.T) {
	now := time.Unix(10_000, 0).UTC()
	direct := &transferDirectFake{}
	lifetimes := make(chan context.Context, 1)
	broker, err := NewFileTransferBroker(transferPreparerFake{direct: direct, lifetime: lifetimes})
	if err != nil {
		t.Fatal(err)
	}
	broker.now = func() time.Time { return now }
	request := localapi.FileTransferRequest{Schema: localapi.FileTransferSchemaV1, MachineID: "machine_1", EnvironmentID: "environment_1", MachineGeneration: 1, OperationID: "operation_1", Credential: "credential_1", AccessSessionID: "access_1", Deadline: now.Add(time.Hour), MaximumBytes: 1 << 20}
	owner := localapi.Peer{UID: 1000, GID: 1000, PID: 41}
	setupCtx, cancelSetup := context.WithCancel(context.Background())
	result, err := broker.PrepareFileTransfer(setupCtx, owner, request)
	if err != nil || result.Handle == "" {
		t.Fatalf("prepare result=%+v err=%v", result, err)
	}
	lifetime := <-lifetimes
	cancelSetup()
	select {
	case <-lifetime.Done():
		t.Fatal("request completion canceled retained transfer lease")
	default:
	}
	if _, err := broker.OpenFileTransferStream(context.Background(), localapi.Peer{UID: 1000, GID: 1000, PID: 42}, result.Handle); err == nil {
		t.Fatal("different PID opened transfer handle")
	}
	stream, err := broker.OpenFileTransferStream(context.Background(), owner, result.Handle)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	if err := broker.ReleaseFileTransfer(owner, result.Handle); err != nil || !direct.closed {
		t.Fatalf("release err=%v closed=%t", err, direct.closed)
	}
	select {
	case <-lifetime.Done():
	default:
		t.Fatal("release did not cancel retained transfer lifetime")
	}
}

func TestFileTransferBrokerExpiresAndBoundsNativeLeases(t *testing.T) {
	now := time.Unix(20_000, 0).UTC()
	directs := make([]*transferDirectFake, 0, maxNativeTransferLeases+1)
	preparer := nativeTransferPreparerFunc(func(context.Context, resolver.ConnectInfo, string) (tunnel.DirectTransferStreamOpener, error) {
		direct := &transferDirectFake{}
		directs = append(directs, direct)
		return direct, nil
	})
	broker, err := NewFileTransferBroker(preparer)
	if err != nil {
		t.Fatal(err)
	}
	broker.now = func() time.Time { return now }
	peer := localapi.Peer{UID: 1000, GID: 1000, PID: 41}
	request := localapi.FileTransferRequest{Schema: localapi.FileTransferSchemaV1, MachineID: "machine_1", EnvironmentID: "environment_1", MachineGeneration: 1, OperationID: "operation_1", Credential: "credential_1", AccessSessionID: "access_1", Deadline: now.Add(time.Hour), MaximumBytes: 1 << 20}
	var first string
	for index := 0; index < maxNativeTransferLeases; index++ {
		request.OperationID = "operation_" + strconv.Itoa(index)
		result, prepareErr := broker.PrepareFileTransfer(context.Background(), peer, request)
		if prepareErr != nil {
			t.Fatal(prepareErr)
		}
		if index == 0 {
			first = result.Handle
		}
	}
	request.OperationID = "operation_overflow"
	if _, err := broker.PrepareFileTransfer(context.Background(), peer, request); err == nil || !directs[len(directs)-1].closed {
		t.Fatal("lease bound did not close rejected native session")
	}
	broker.now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, err := broker.OpenFileTransferStream(context.Background(), peer, first); !errors.Is(err, localapi.ErrPermission) || !directs[0].closed {
		t.Fatalf("expired lease err=%v closed=%t", err, directs[0].closed)
	}
}

type nativeTransferPreparerFunc func(context.Context, resolver.ConnectInfo, string) (tunnel.DirectTransferStreamOpener, error)

func (f nativeTransferPreparerFunc) PrepareNativeFileTransfer(ctx context.Context, info resolver.ConnectInfo, operationID string) (tunnel.DirectTransferStreamOpener, error) {
	return f(ctx, info, operationID)
}
