package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

type execCancelRemote struct {
	execInputTestConn
	events          chan tunnel.ExecEvent
	release, closed chan struct{}
	once            sync.Once
}

func (r *execCancelRemote) Events() <-chan tunnel.ExecEvent { return r.events }
func (r *execCancelRemote) Cancel() error {
	select {
	case <-r.release:
		return nil
	case <-r.closed:
		return net.ErrClosed
	}
}
func (r *execCancelRemote) Wait() (int, error) {
	return 130, &tunnel.RemoteExecError{Code: "exec_canceled"}
}
func (r *execCancelRemote) Detach() error { r.once.Do(func() { close(r.closed) }); return nil }
func (r *execCancelRemote) Close() error  { return r.Detach() }

func TestExecCancellationRetainsResultAfterOriginalAttachmentEnds(t *testing.T) {
	var descriptors, opens atomic.Int32
	var operation atomic.Value
	configPath, paths, cfg := execCommandBrokerFixture(t, &descriptors)
	output := &execRecoveryOutput{observed: make(chan struct{})}
	initial := make(chan tunnel.ExecEvent, 2)
	firstDone := make(chan struct{})
	release := make(chan struct{})
	var closeInitial sync.Once
	var releaseOnce sync.Once
	t.Cleanup(func() { closeInitial.Do(func() { close(initial) }); releaseOnce.Do(func() { close(release) }) })
	cfg.PeerStreams = execRecoveryBroker(func(ctx context.Context, _ localapi.Peer, r localapi.PeerStreamRequest) (net.Conn, error) {
		var request tunnel.ExecRequest
		if err := json.Unmarshal(r.Payload, &request); err != nil {
			return nil, err
		}
		attempt := opens.Add(1)
		if attempt == 1 {
			operation.Store(request.OperationID)
		} else if request.OperationID != operation.Load().(string) || r.Credential != "renewed_1" {
			t.Error("cancellation changed execution identity or skipped fresh authorization")
			return nil, errors.New("invalid cancellation binding")
		}
		client, server := net.Pipe()
		if attempt == 1 {
			initial <- tunnel.ExecEvent{OperationID: request.OperationID, State: "started"}
			initial <- tunnel.ExecEvent{OperationID: request.OperationID, EventSequence: 1, Stream: "stdout", Data: []byte("before")}
			remote := &execRecoveryRemote{events: initial}
			go func() { defer close(firstDone); _ = tunnel.ServeLocalPeerConn(t.Context(), server, remote) }()
			return client, nil
		}
		// Reattaching for cancellation retires the old output attachment first.
		closeInitial.Do(func() { close(initial) })
		select {
		case <-firstDone:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		remote := &execCancelRemote{events: make(chan tunnel.ExecEvent, 2), release: release, closed: make(chan struct{})}
		remote.events <- tunnel.ExecEvent{OperationID: request.OperationID, State: "started"}
		go func() {
			defer close(remote.events)
			select {
			case <-release:
				remote.events <- tunnel.ExecEvent{OperationID: request.OperationID, State: "canceled", ErrorCode: "exec_canceled", Result: &tunnel.ExecResult{Code: 130}}
			case <-remote.closed:
			}
		}()
		go func() { _ = tunnel.ServeLocalPeerConn(t.Context(), server, remote) }()
		// The replacement result remains asynchronous to the retired original stream.
		go func() {
			select {
			case <-firstDone:
			case <-t.Context().Done():
			}
			releaseOnce.Do(func() { close(release) })
		}()
		return client, nil
	})
	server, err := localapi.NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, stopServer := context.WithCancel(t.Context())
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(serverCtx) }()
	defer stopLocalDaemonReadinessServer(t, stopServer, serverDone)
	waitForCommandSocket(t, paths.SocketPath)
	command := newRootCommand()
	command.SetOut(output)
	command.SetArgs([]string{"--config", configPath, "--no-customization", "exec", "target", "--", "fixture-command"})
	commandCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		select {
		case <-output.observed:
			cancel()
		case <-commandCtx.Done():
		}
	}()
	done := make(chan error, 1)
	go func() { done <- command.ExecuteContext(commandCtx) }()
	select {
	case err := <-done:
		var exit interface{ ExitCode() int }
		var remote *tunnel.RemoteExecError
		if opens.Load() != 2 || descriptors.Load() != 1 || !errors.As(err, &exit) || exit.ExitCode() != 130 || !errors.As(err, &remote) || remote.Code != "exec_canceled" || output.String() != "before" {
			t.Fatalf("known cancellation was replaced by carrier loss: opens=%d err=%v output=%q", opens.Load(), err, output.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not finish")
	}
}
