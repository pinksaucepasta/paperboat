//go:build darwin || linux

package tunnel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

func TestLocalPeerDaemonRestartRemainsReconnectable(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "pb-reconnect-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	client, err := localapi.NewClient(root+"/daemon.sock", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	peer := LocalPeerTunnel{Client: client}
	info := resolver.ConnectInfo{MachineID: "machine_1", MachineGeneration: 1, Terminal: &resolver.TerminalTarget{
		EnvironmentID: "environment_1", SessionID: "session_1",
		Auth: resolver.AuthTarget{Token: "credential", ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339)},
	}}
	if _, err = peer.Dial(context.Background(), info); !FallbackEligible(err) {
		t.Fatalf("temporary daemon absence stopped reconnect: %v", err)
	} else {
		var staged interface{ DiagnosticStage() string }
		var coded interface{ DiagnosticCode() string }
		if !errors.Is(err, localapi.ErrTransportUnavailable) ||
			!errors.As(err, &staged) || staged.DiagnosticStage() != "local_gateway" ||
			!errors.As(err, &coded) || coded.DiagnosticCode() != "local_access_failed" ||
			err.Error() != "local terminal transport unavailable" {
			t.Fatalf("local transport failure lost cause or safe classification: %T %v", err, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = peer.Dial(ctx, info); FallbackEligible(err) {
		t.Fatal("canceled connection remained retryable")
	}
}

type reconnectAfterInputConn struct {
	*reconnectTestConn
	lost <-chan struct{}
}

func (c *reconnectAfterInputConn) Wait() (int, error) { <-c.lost; return 1, ErrTransportLost }

type nativeOperationalStop struct{ error }

func (e nativeOperationalStop) Unwrap() error                { return e.error }
func (nativeOperationalStop) NativeTransportRetryable() bool { return true }

func TestLocalPeerNativeTransportRecoveryPreservesRetryabilityWithoutInputReplay(t *testing.T) {
	expired, expire := context.WithCancelCause(context.Background())
	expire(context.DeadlineExceeded)
	canceled, cancelCaller := context.WithCancel(context.Background())
	cancelCaller()
	for _, test := range []struct {
		name    string
		failure error
		recover bool
	}{
		{"credential expires during setup", &terminalTransportError{transport: "native application", cause: contextOperationError(expired)}, true},
		{"caller cancels setup", &terminalTransportError{transport: "native application", cause: contextOperationError(canceled)}, false},
		{"owned native transport cancellation", &terminalTransportError{transport: "native application", cause: nativeOperationalStop{context.Canceled}}, true},
		{"owned native stop with inner refusal", &terminalTransportError{transport: "native application", cause: errors.Join(nativeOperationalStop{context.Canceled}, &helperRemoteError{Code: "permission_denied", Retryable: false})}, false},
		{"native connection closing", &terminalTransportError{transport: "QUIC", cause: net.ErrClosed}, true},
		{"terminal service restarting", &terminalTransportError{transport: "native application", cause: &helperRemoteError{Code: "unavailable", Retryable: true}}, true},
		{"terminal service setup deadline", &terminalTransportError{transport: "native application", cause: &helperRemoteError{Code: "deadline_exceeded", Retryable: true}}, true},
		{"authorization refused", &terminalTransportError{transport: "native application", cause: &helperRemoteError{Code: "permission_denied", Retryable: false}}, false},
		{"arbitrary broker failure", errors.New("broker failed"), false},
		{"control credential rejected", &terminalTransportError{transport: "native runtime", cause: &api.APIError{Status: 401}}, false},
		{"control authorization rejected", &terminalTransportError{transport: "native runtime", cause: &api.APIError{Status: 403}}, false},
		{"peer admission refused", &terminalTransportError{transport: "native application", cause: tailnet.ErrAdmission}, false},
		{"invalid native welcome", &terminalTransportError{transport: "native application", cause: errInvalidNativeWelcome}, false},
		{"environment unavailable", &terminalTransportError{transport: "native application", cause: &helperRemoteError{Code: "environment_unavailable", Retryable: true}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			root, err := os.MkdirTemp("/tmp", "pb-peer-recovery-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			socket := root + "/peer.sock"
			var attempts atomic.Int32
			recovered := &reconnectTestConn{Reader: bytes.NewReader([]byte("recovered"))}
			broker := localPeerTunnelBroker(func(c context.Context, _ localapi.Peer, r localapi.PeerStreamRequest) (net.Conn, error) {
				if r.Consumer != "terminal" || r.OperationID != "session_1" {
					return nil, ErrPeerTerminalInvalid
				}
				attempt := attempts.Add(1)
				if r.Credential != fmt.Sprintf("authorized-%d", attempt) {
					return nil, &helperRemoteError{Code: "permission_denied", Retryable: false}
				}
				if attempt == 1 || !test.recover {
					return nil, test.failure
				}
				client, server := net.Pipe()
				go func() { _ = ServeLocalPeerConn(ctx, server, recovered) }()
				return client, nil
			})
			server, err := localapi.NewServer(localapi.ServerConfig{SocketPath: socket, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), Source: localPeerTunnelSnapshotSource{}, PeerStreams: broker})
			if err != nil {
				t.Fatal(err)
			}
			serverCtx, stop := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- server.Run(serverCtx) }()
			defer func() {
				stop()
				e := <-done
				if e != nil && !errors.Is(e, context.Canceled) {
					t.Error(e)
				}
			}()
			for {
				if _, e := os.Stat(socket); e == nil {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(time.Millisecond):
				}
			}
			client, err := localapi.NewClient(socket, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			peer := LocalPeerTunnel{Client: client}
			info := resolver.ConnectInfo{MachineID: "machine_1", MachineGeneration: 1, Terminal: &resolver.TerminalTarget{EnvironmentID: "environment_1", SessionID: "session_1", Auth: resolver.AuthTarget{Token: "credential", ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339)}}}
			lost := make(chan struct{})
			initial := &reconnectAfterInputConn{reconnectTestConn: &reconnectTestConn{Reader: bytes.NewReader(nil)}, lost: lost}
			var resolutions int
			conn := NewReconnectingConn(ctx, initial, 3, 0, func(c context.Context) (Conn, error) {
				resolutions++
				info.Terminal.Auth.Token = fmt.Sprintf("authorized-%d", resolutions)
				n, e := peer.Dial(c, info)
				if e != nil && !FallbackEligible(e) {
					return nil, StopReconnect(e)
				}
				return n, e
			})
			defer conn.Close()
			if _, err := conn.Write([]byte("already accepted input")); err != nil {
				t.Fatal(err)
			}
			close(lost)
			payload, readErr := io.ReadAll(conn)
			code, waitErr := conn.Wait()
			if test.recover {
				if readErr != nil || waitErr != nil || code != 0 || string(payload) != "recovered" || attempts.Load() != 2 {
					t.Fatalf("recovery code=%d read=%v wait=%v attempts=%d", code, readErr, waitErr, attempts.Load())
				}
				if initial.writes.String() != "already accepted input" || recovered.writes.Len() != 0 {
					t.Fatal("terminal input was replayed")
				}
			} else if waitErr == nil || attempts.Load() != 1 {
				t.Fatalf("authoritative failure retried: %v attempts=%d", waitErr, attempts.Load())
			}
		})
	}
}
