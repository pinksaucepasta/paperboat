//go:build darwin || linux

package localapi

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// An update replaces the daemon and ends the streams it carries, so the daemon reports the open
// SSH-family streams: they count while open and stop counting when either side closes.
func TestLocalAPIReportsOpenSSHStreams(t *testing.T) {
	socket := filepath.Join(localAPITestDir(t), "connections.sock")
	server, err := NewServer(ServerConfig{SocketPath: socket, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(),
		Source: snapshotSourceFunc(func(context.Context) (Snapshot, error) { return validSnapshot(), nil }),
		PeerStreams: peerStreamBrokerFunc(func(context.Context, Peer, PeerStreamRequest) (net.Conn, error) {
			local, remote := net.Pipe()
			go func() { defer remote.Close(); _, _ = remote.Read(make([]byte, 1)) }()
			return local, nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = server.Run(ctx) }()
	waitForSocket(t, socket)
	client, err := NewClient(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := client.OpenConnections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ssh := func(consumer string) net.Conn {
		request, err := NewPeerStreamRequest("machine_1", "environment_1", 2, consumer, "operation_1", "credential", time.Now().Add(time.Minute), 1024, nil)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := client.OpenPeerStream(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		return stream
	}
	eventually := func(want int) {
		t.Helper()
		for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			got, err := client.OpenConnections(ctx)
			if err == nil && got.SSH == baseline.SSH+want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("open ssh streams = %+v (err %v), want baseline %d + %d", got, err, baseline.SSH, want)
			}
		}
	}
	first, second := ssh("ssh"), ssh("ssh")
	other := ssh("exec") // an exec is not an ssh connection
	eventually(2)
	_ = first.Close()
	eventually(1)
	_ = second.Close()
	_ = other.Close()
	eventually(0)
}

// pb logout must end every managed session this computer holds: a relayed close from the host can be lost, so the
// daemon closes its own open peer streams once the sign-in is gone (device: a plain ssh survived logout until killed).
func TestLocalAPIClosesOpenPeerStreamsWhenTheSignInEnds(t *testing.T) {
	var ended atomic.Bool
	socket := filepath.Join(localAPITestDir(t), "signout.sock")
	server, err := NewServer(ServerConfig{SocketPath: socket, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(),
		Source:           snapshotSourceFunc(func(context.Context) (Snapshot, error) { return validSnapshot(), nil }),
		CredentialsEnded: ended.Load, CredentialsEndedPoll: 20 * time.Millisecond,
		PeerStreams: peerStreamBrokerFunc(func(context.Context, Peer, PeerStreamRequest) (net.Conn, error) {
			local, remote := net.Pipe() // a host that never answers and never closes
			t.Cleanup(func() { _ = remote.Close() })
			return local, nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = server.Run(ctx) }()
	waitForSocket(t, socket)
	client, err := NewClient(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	request, err := NewPeerStreamRequest("machine_1", "environment_1", 2, "ssh", "operation_1", "credential", time.Now().Add(time.Minute), 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.OpenPeerStream(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	read := make(chan error, 1)
	go func() { _, err := stream.Read(make([]byte, 1)); read <- err }()
	select {
	case err := <-read:
		t.Fatalf("the stream ended while signed in: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	ended.Store(true)
	select {
	case <-read:
	case <-time.After(3 * time.Second):
		t.Fatal("an open peer stream outlived the sign-in")
	}
}
