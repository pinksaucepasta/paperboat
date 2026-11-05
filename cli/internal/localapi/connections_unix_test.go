//go:build darwin || linux

package localapi

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
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

// A daemon generation that drains on SIGQUIT stops its local API, but an exec already running through
// it keeps flowing (no end of stream, no redial) and counts as held until it closes, so the drainer
// waits for it (pbvm1 2026-11-05: an exec redialed across a daemon restart).
func TestExecInFlightOutlivesTheLocalAPIStop(t *testing.T) {
	socket := filepath.Join(localAPITestDir(t), "drain.sock")
	server, err := NewServer(ServerConfig{SocketPath: socket, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(),
		Source: snapshotSourceFunc(func(context.Context) (Snapshot, error) { return validSnapshot(), nil }),
		PeerStreams: peerStreamBrokerFunc(func(context.Context, Peer, PeerStreamRequest) (net.Conn, error) {
			local, remote := net.Pipe()
			go func() { defer remote.Close(); _, _ = io.Copy(remote, remote) }()
			return local, nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stopLocalAPI := context.WithCancel(t.Context())
	defer stopLocalAPI()
	stopped := make(chan struct{})
	go func() { defer close(stopped); _ = server.Run(ctx) }()
	waitForSocket(t, socket)
	client, err := NewClient(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	baseline := liveconnections.Held()
	request, err := NewPeerStreamRequest("machine_1", "environment_1", 2, "exec", "operation_1", "credential", time.Now().Add(time.Minute), 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	exec, err := client.OpenPeerStream(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Close()
	echo := func(message string) {
		t.Helper()
		_ = exec.SetDeadline(time.Now().Add(time.Second))
		if _, err := exec.Write([]byte(message)); err != nil {
			t.Fatalf("exec write after the local API stop: %v", err)
		}
		got := make([]byte, len(message))
		if _, err := io.ReadFull(exec, got); err != nil || string(got) != message {
			t.Fatalf("exec read after the local API stop: %q, %v", got, err)
		}
	}
	echo("before")
	stopLocalAPI()
	<-stopped
	for range 5 {
		echo("during drain")
		time.Sleep(50 * time.Millisecond)
	}
	if held := liveconnections.Held(); held < baseline+1 {
		t.Fatalf("held connections = %d, want the exec counted (baseline %d)", held, baseline)
	}
	_ = exec.Close()
	for deadline := time.Now().Add(2 * time.Second); liveconnections.Held() > baseline; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("closed exec still held: %d > %d", liveconnections.Held(), baseline)
		}
	}
}
