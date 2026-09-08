package server

import (
	"context"
	"encoding/json"
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
