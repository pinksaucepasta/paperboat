package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
)

func TestServeNativePrivateTCPForwardsHalfClose(t *testing.T) {
	now := time.Now().UTC()
	originListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer originListener.Close()
	originDone := make(chan struct{})
	go func() {
		connection, acceptErr := originListener.Accept()
		if acceptErr == nil {
			payload, _ := io.ReadAll(connection)
			_, _ = connection.Write(append([]byte("postgres:"), payload...))
			_ = connection.Close()
		}
		close(originDone)
	}()
	clientListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer clientListener.Close()
	user, err := net.Dial("tcp4", clientListener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	host, err := clientListener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	binding := nativeprivate.Binding{Schema: nativeprivate.SchemaV1, ResourceKind: "tunnel", ResourceID: "tun_1", ResourceGeneration: 2, RouteID: "route_1", RouteGeneration: 3, TargetGeneration: 4, OwnerEndpointID: "machine_1", Protocol: "tcp", TargetScheme: "tcp", TargetAddress: originListener.Addr().String(), ExpiresAt: now.Add(time.Minute)}
	target, _ := json.Marshal(binding)
	header, err := streamauth.NewNativePrivate("operation_1", "private_tcp", "stream_1", "credential", binding.ExpiresAt, 1<<20, target)
	if err != nil {
		t.Fatal(err)
	}
	revoked := make(chan struct{})
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- ServeNativePrivateTCP(context.Background(), header, host, func(context.Context, nativeprivate.Binding) (time.Time, <-chan struct{}, error) {
			return binding.ExpiresAt, revoked, nil
		}, func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		})
	}()
	var ready [1]byte
	if _, err = io.ReadFull(user, ready[:]); err != nil || ready[0] != 0 {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
	if _, err = user.Write([]byte("query")); err != nil {
		t.Fatal(err)
	}
	if err = user.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(user)
	if err != nil || string(response) != "postgres:query" {
		t.Fatalf("response=%q err=%v", response, err)
	}
	_ = user.Close()
	if err = <-serveDone; err != nil {
		t.Fatal(err)
	}
	<-originDone
	close(revoked)
}

func TestServeNativePrivateTCPClassifiesAuthorityAndTargetFailures(t *testing.T) {
	binding := nativeprivate.Binding{Schema: nativeprivate.SchemaV1, ResourceKind: "tunnel", ResourceID: "tun_1", ResourceGeneration: 1, RouteID: "route_1", RouteGeneration: 1, TargetGeneration: 1, OwnerEndpointID: "machine_1", Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:5432", ExpiresAt: time.Now().UTC().Add(time.Minute)}
	target, _ := json.Marshal(binding)
	header, err := streamauth.NewNativePrivate("operation_failure", "private_tcp", "stream_failure", "credential", binding.ExpiresAt, 1024, target)
	if err != nil {
		t.Fatal(err)
	}

	for _, testCase := range []struct {
		name  string
		stage string
		code  string
		serve func(net.Conn, error) error
	}{
		{name: "authority", stage: "peer_authority", code: "peer_authority_failed", serve: func(client net.Conn, cause error) error {
			return ServeNativePrivateTCP(context.Background(), header, client, func(context.Context, nativeprivate.Binding) (time.Time, <-chan struct{}, error) {
				return time.Time{}, nil, cause
			}, func(context.Context, string, string) (net.Conn, error) {
				t.Fatal("dial ran after failed authority check")
				return nil, nil
			})
		}},
		{name: "target dial", stage: "target_connect", code: "native_private_failed", serve: func(client net.Conn, cause error) error {
			return ServeNativePrivateTCP(context.Background(), header, client, func(context.Context, nativeprivate.Binding) (time.Time, <-chan struct{}, error) {
				return binding.ExpiresAt, nil, nil
			}, func(context.Context, string, string) (net.Conn, error) { return nil, cause })
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client, peer := net.Pipe()
			defer client.Close()
			defer peer.Close()
			cause := errors.New("private network detail")
			got := testCase.serve(client, cause)
			var staged interface{ DiagnosticStage() string }
			var coded interface{ DiagnosticCode() string }
			if !errors.Is(got, cause) || !errors.As(got, &staged) || staged.DiagnosticStage() != testCase.stage ||
				!errors.As(got, &coded) || coded.DiagnosticCode() != testCase.code || got.Error() != "native private operation failed" {
				t.Fatalf("classified failure = %T %v", got, got)
			}
		})
	}
}

func TestServeNativePrivateTCPRevocationClosesActiveStream(t *testing.T) {
	now := time.Now().UTC()
	originListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer originListener.Close()
	originAccepted := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := originListener.Accept()
		if acceptErr == nil {
			originAccepted <- connection
		}
	}()
	clientListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer clientListener.Close()
	user, err := net.Dial("tcp4", clientListener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer user.Close()
	host, err := clientListener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	binding := nativeprivate.Binding{Schema: nativeprivate.SchemaV1, ResourceKind: "tunnel", ResourceID: "tun_1", ResourceGeneration: 2, RouteID: "route_1", RouteGeneration: 3, TargetGeneration: 4, OwnerEndpointID: "machine_1", Protocol: "tcp", TargetScheme: "tcp", TargetAddress: originListener.Addr().String(), ExpiresAt: now.Add(time.Minute)}
	target, _ := json.Marshal(binding)
	header, err := streamauth.NewNativePrivate("operation_2", "private_tcp", "stream_2", "credential", binding.ExpiresAt, 1<<20, target)
	if err != nil {
		t.Fatal(err)
	}
	revoked := make(chan struct{})
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- ServeNativePrivateTCP(context.Background(), header, host, func(context.Context, nativeprivate.Binding) (time.Time, <-chan struct{}, error) {
			return binding.ExpiresAt, revoked, nil
		}, func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		})
	}()
	var ready [1]byte
	if _, err = io.ReadFull(user, ready[:]); err != nil || ready[0] != 0 {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
	origin := <-originAccepted
	defer origin.Close()
	close(revoked)
	_ = user.SetReadDeadline(time.Now().Add(time.Second))
	if _, err = user.Read(ready[:]); err == nil {
		t.Fatal("active stream remained open after revocation")
	}
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("handler did not stop after revocation")
	}
}

func TestServeNativePrivateTCPCancelsBeforeReadiness(t *testing.T) {
	for _, stage := range []string{"dial", "readiness"} {
		t.Run(stage, func(t *testing.T) {
			binding := nativeprivate.Binding{Schema: nativeprivate.SchemaV1, ResourceKind: "tunnel", ResourceID: "tun_1", ResourceGeneration: 1, RouteID: "route_1", RouteGeneration: 1, TargetGeneration: 1, OwnerEndpointID: "machine_1", Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:3000", ExpiresAt: time.Now().Add(time.Minute)}
			target, _ := json.Marshal(binding)
			header, err := streamauth.NewNativePrivate("operation_1", "private_tcp", "stream_1", "credential", binding.ExpiresAt, 1<<20, target)
			if err != nil {
				t.Fatal(err)
			}
			host, user := net.Pipe()
			defer host.Close()
			defer user.Close()
			origin, remote := net.Pipe()
			defer origin.Close()
			defer remote.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dialed := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- ServeNativePrivateTCP(ctx, header, host, func(context.Context, nativeprivate.Binding) (time.Time, <-chan struct{}, error) {
					return binding.ExpiresAt, nil, nil
				}, func(ctx context.Context, _, _ string) (net.Conn, error) {
					close(dialed)
					if stage == "dial" {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					return origin, nil
				})
			}()
			<-dialed
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("canceled setup reported success")
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation did not interrupt setup")
			}
		})
	}
}
