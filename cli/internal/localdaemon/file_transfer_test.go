package localdaemon

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

type transferPreparerFake struct {
	direct   *transferDirectFake
	lifetime chan context.Context
	paths    *pathFake
}

func (f transferPreparerFake) PathEligibility(ctx context.Context, machineID string) (tailnet.PathKind, <-chan struct{}) {
	if f.paths == nil {
		return tailnet.PathDirect, ctx.Done()
	}
	return f.paths.current(ctx)
}

// pathFake is the engine's path to one machine; set changes it and wakes watchers.
type pathFake struct {
	mu      sync.Mutex
	kind    tailnet.PathKind
	changed chan struct{}
}

func newPathFake(kind tailnet.PathKind) *pathFake {
	return &pathFake{kind: kind, changed: make(chan struct{})}
}

func (p *pathFake) current(context.Context) (tailnet.PathKind, <-chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.kind, p.changed
}

func (p *pathFake) set(kind tailnet.PathKind) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.kind = kind
	close(p.changed)
	p.changed = make(chan struct{})
}

func (f transferPreparerFake) PrepareNativeFileTransfer(lifetime context.Context, _ resolver.ConnectInfo, _ string) (tunnel.DirectTransferStreamOpener, error) {
	if f.lifetime != nil {
		f.lifetime <- lifetime
	}
	return f.direct, nil
}

type transferDirectFake struct {
	closed bool
	// open keeps the far end of each stream open, draining what is written.
	open bool
}

func (f *transferDirectFake) Close() error { f.closed = true; return nil }
func (f *transferDirectFake) OpenTransferStream(context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	if !f.open {
		_ = server.Close()
		return client, nil
	}
	go func() { _, _ = io.Copy(io.Discard, server) }()
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
	t.Cleanup(func() { _ = broker.Close() })
	broker.now = func() time.Time { return now }
	request := localapi.FileTransferRequest{Schema: localapi.FileTransferSchemaV1, MachineID: "machine_1", EnvironmentID: "environment_1", MachineGeneration: 1, OperationID: "operation_1", Credential: "credential_1", AccessSessionID: "access_1", Deadline: now.Add(time.Hour), MaximumBytes: 1 << 20}
	owner := localapi.Peer{UID: 1000, GID: 1000, PID: 41}
	setupCtx, cancelSetup := context.WithCancel(supportref.WithContext(context.Background(), supportref.New()))
	result, err := broker.PrepareFileTransfer(setupCtx, owner, request)
	if err != nil || result.Handle == "" {
		t.Fatalf("prepare result=%+v err=%v", result, err)
	}
	lifetime := <-lifetimes
	if supportref.FromContext(lifetime) != supportref.FromContext(setupCtx) {
		t.Fatal("transfer lifetime lost invocation reference")
	}
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

func TestFileTransferBrokerOutlivesCredentialAndKeepsPeerAndCapacityBounds(t *testing.T) {
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
	t.Cleanup(func() { _ = broker.Close() })
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
	stream, err := broker.OpenFileTransferStream(context.Background(), peer, first)
	if err != nil || directs[0].closed {
		t.Fatalf("credential expiry closed admitted lease: %v", err)
	}
	stream.Close()
	if err := broker.ReleaseFileTransfer(peer, first); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.OpenFileTransferStream(context.Background(), peer, first); !errors.Is(err, localapi.ErrPermission) {
		t.Fatal("released lease remained usable", err)
	}
}

type nativeTransferPreparerFunc func(context.Context, resolver.ConnectInfo, string) (tunnel.DirectTransferStreamOpener, error)

func (f nativeTransferPreparerFunc) PrepareNativeFileTransfer(ctx context.Context, info resolver.ConnectInfo, operationID string) (tunnel.DirectTransferStreamOpener, error) {
	return f(ctx, info, operationID)
}

func (nativeTransferPreparerFunc) PathEligibility(ctx context.Context, _ string) (tailnet.PathKind, <-chan struct{}) {
	return tailnet.PathDirect, ctx.Done()
}

// PLAN 3b File transfer modes: an Unlimited stream never uses a Paperboat-hosted relay. It is refused while the
// only path is one, opens on a direct path, and is closed the moment the path moves to a hosted relay.
func TestUnlimitedTransferStreamsUseOnlyDirectOrOwnRelayPaths(t *testing.T) {
	now := time.Unix(10_000, 0).UTC()
	paths := newPathFake(tailnet.PathHostedRelay)
	broker, err := NewFileTransferBroker(transferPreparerFake{direct: &transferDirectFake{open: true}, paths: paths})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	broker.now = func() time.Time { return now }
	owner := localapi.Peer{UID: 1000, GID: 1000, PID: 41}
	request := localapi.FileTransferRequest{Schema: localapi.FileTransferSchemaV1, MachineID: "machine_1", EnvironmentID: "environment_1", MachineGeneration: 1, OperationID: "operation_1", Credential: "credential_1", AccessSessionID: "access_1", Deadline: now.Add(time.Hour), MaximumBytes: 1 << 20, TransferMode: "unlimited"}
	result, err := broker.PrepareFileTransfer(context.Background(), owner, request)
	if err != nil {
		t.Fatal(err)
	}
	var waiting *localapi.NoEligiblePathError
	if _, err := broker.OpenFileTransferStream(context.Background(), owner, result.Handle); !errors.As(err, &waiting) || waiting.Path != "hosted_relay" {
		t.Fatalf("a stream opened over a hosted relay: %v", err)
	}
	paths.set(tailnet.PathDirect)
	stream, err := broker.OpenFileTransferStream(context.Background(), owner, result.Handle)
	if err != nil {
		t.Fatalf("a direct path was refused: %v", err)
	}
	if _, err := stream.Write([]byte("x")); err != nil {
		t.Fatalf("the direct stream did not carry data: %v", err)
	}
	paths.set(tailnet.PathHostedRelay)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := stream.Write([]byte("x")); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stream stayed open after the path moved to a hosted relay")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
