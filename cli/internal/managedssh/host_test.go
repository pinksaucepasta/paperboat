package managedssh

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestHostReconcilesAndServesGenerationFencedTarget(t *testing.T) {
	listener := loopbackListener(t)
	defer listener.Close()
	go echoConnections(listener)
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	const sourceA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const sourceB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	host, err := NewHost(HostConfig{ProbeTimeout: time.Second, DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := host.CheckSourceRetirement(sourceA); !errors.Is(err, ErrSSHHostStale) {
		t.Fatalf("retired source without a published current identity: %v", err)
	}
	target, err := host.ReconcileSourceTarget(context.Background(), 3, port, sourceA)
	if err != nil || target.Generation != 3 || target.SourceSHA256 != sourceA || target.Port != port || target.Readiness.Target.Port != port {
		t.Fatalf("target=%+v error=%v", target, err)
	}
	if _, err := host.ReconcileSourceTarget(context.Background(), 3, port, "not-a-source-hash"); !errors.Is(err, ErrSSHHostInvalid) {
		t.Fatalf("invalid source generation: %v", err)
	}
	if replay, err := host.ReconcileSourceTarget(context.Background(), 3, port, sourceA); err != nil || replay != target {
		t.Fatalf("replay=%+v error=%v", replay, err)
	}
	if _, err := host.ReconcileSourceTarget(context.Background(), 3, port, sourceB); !errors.Is(err, ErrSSHHostStale) {
		t.Fatalf("same epoch changed source: %v", err)
	}
	if _, err := host.ReconcileTarget(context.Background(), 2, port); !errors.Is(err, ErrSSHHostStale) {
		t.Fatalf("stale reconcile error=%v", err)
	}
	client, server := net.Pipe()
	result := make(chan error, 1)
	go func() {
		_, err := host.Serve(context.Background(), 3, server)
		result <- err
	}()
	message := []byte("SSH-2.0-paperboat-test\r\n")
	if _, err := client.Write(message); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(message))
	if _, err := io.ReadFull(client, got); err != nil || string(got) != string(message) {
		t.Fatalf("echo=%q error=%v", got, err)
	}
	if err := host.Retire(); !errors.Is(err, ErrSSHSessionsOpen) {
		t.Fatalf("retire active connection: %v", err)
	}
	if err := host.CheckSourceRetirement(sourceA); !errors.Is(err, ErrSSHHostStale) {
		t.Fatalf("retire current source with active connection: %v", err)
	}
	if _, err := host.ReconcileSourceTarget(context.Background(), 4, port, sourceA); err != nil {
		t.Fatal(err)
	}
	if err := host.CheckSourceRetirement(sourceA); !errors.Is(err, ErrSSHHostStale) {
		t.Fatalf("same source on newer worker epoch was retireable: %v", err)
	}
	// The accepted bridge keeps its original socket after an epoch publication.
	if _, err := client.Write(message); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(client, got); err != nil || string(got) != string(message) {
		t.Fatalf("echo after target change=%q error=%v", got, err)
	}
	if _, err := host.Serve(context.Background(), 3, pipeStream(t)); !errors.Is(err, ErrSSHHostStale) {
		t.Fatalf("admit old target after publication: %v", err)
	}
	_ = client.Close()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if err := host.CheckSourceRetirement(sourceA); !errors.Is(err, ErrSSHHostStale) {
		t.Fatalf("current source became retireable after old epoch closed: %v", err)
	}

	// A distinct new source may publish while an old-source connection remains
	// alive, but its source slot cannot retire until that accepted bridge exits.
	left2, right2 := net.Pipe()
	defer left2.Close()
	result2 := make(chan error, 1)
	go func() {
		_, err := host.Serve(context.Background(), 4, right2)
		result2 <- err
	}()
	if _, err := left2.Write(message); err != nil {
		t.Fatal(err)
	}
	got = make([]byte, len(message))
	if _, err := io.ReadFull(left2, got); err != nil || string(got) != string(message) {
		t.Fatalf("second source echo=%q error=%v", got, err)
	}
	if _, err := host.ReconcileSourceTarget(context.Background(), 5, port, sourceB); err != nil {
		t.Fatal(err)
	}
	if err := host.CheckSourceRetirement(sourceA); !errors.Is(err, ErrSSHSessionsOpen) {
		t.Fatalf("old source with live connection: %v", err)
	}
	if _, err := left2.Write(message); err != nil {
		t.Fatal(err)
	}
	got = make([]byte, len(message))
	if _, err := io.ReadFull(left2, got); err != nil || string(got) != string(message) {
		t.Fatalf("accepted old-source bridge after source publication=%q error=%v", got, err)
	}
	_ = left2.Close()
	if err := <-result2; err != nil {
		t.Fatal(err)
	}
	if err := host.CheckSourceRetirement(sourceA); err != nil {
		t.Fatalf("closed old source: %v", err)
	}
	if err := host.CheckSourceRetirement(sourceB); !errors.Is(err, ErrSSHHostStale) {
		t.Fatalf("current source retirement: %v", err)
	}
	if err := host.Retire(); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Serve(context.Background(), 5, pipeStream(t)); !errors.Is(err, ErrSSHHostStale) {
		t.Fatalf("retired carrier serve error=%v", err)
	}
	if _, ok := host.Target(); ok {
		t.Fatal("retired carrier advertises an accepting target")
	}
	if _, err := host.ReconcileTarget(context.Background(), 6, port); !errors.Is(err, ErrSSHHostStale) {
		t.Fatalf("retired carrier target publication: %v", err)
	}
	if err := host.ResumeAdmissions(); err != nil {
		t.Fatal(err)
	}
	if target, ok := host.Target(); !ok || target.Generation != 5 || target.SourceSHA256 != sourceB {
		t.Fatalf("resumed target=%+v ready=%v", target, ok)
	}
}

// Disable cuts open sessions and fences new ones until Enable.
func TestHostDisableEndsOpenSessions(t *testing.T) {
	listener := loopbackListener(t)
	defer listener.Close()
	go echoConnections(listener)
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	host, _ := NewHost(HostConfig{ProbeTimeout: time.Second, DialTimeout: time.Second})
	if _, err := host.ReconcileTarget(context.Background(), 1, port); err != nil {
		t.Fatal(err)
	}
	firstClient, firstServer := net.Pipe()
	defer firstClient.Close()
	firstDone := make(chan error, 1)
	go func() { _, err := host.Serve(context.Background(), 1, firstServer); firstDone <- err }()
	deadline := time.Now().Add(time.Second)
	for host.OpenStreams() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	secondClient, secondServer := net.Pipe()
	defer secondClient.Close()
	defer secondServer.Close()
	disableCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := host.Disable(disableCtx); err != nil {
		t.Fatal(err)
	}
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("revoked bridge result: %v", err)
	}
	if _, ok := host.Target(); ok {
		t.Fatal("disabled SSH still advertises a target")
	}
	if err := host.ResumeAdmissions(); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Serve(context.Background(), 1, secondServer); !errors.Is(err, ErrSSHHostStale) {
		t.Fatalf("update rollback bypassed disabled policy: %v", err)
	}
	if err := host.Retire(); err != nil {
		t.Fatal(err)
	}
	if err := host.Enable(); err != nil {
		t.Fatal(err)
	}
	if _, ok := host.Target(); ok {
		t.Fatal("policy enable bypassed native retirement fence")
	}
	if err := host.ResumeAdmissions(); err != nil {
		t.Fatal(err)
	}
	if target, ok := host.Target(); !ok || target.Generation != 1 {
		t.Fatalf("reenabled target=%+v ready=%v", target, ok)
	}
}

func loopbackListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

func echoConnections(listener net.Listener) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			_, _ = io.Copy(connection, connection)
			_ = connection.Close()
		}()
	}
}

func pipeStream(t *testing.T) io.ReadWriteCloser {
	t.Helper()
	left, right := net.Pipe()
	t.Cleanup(func() { _ = left.Close(); _ = right.Close() })
	return left
}
