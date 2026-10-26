package managedssh

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type auditLog struct {
	mu     sync.Mutex
	events []SessionEvent
	fail   error
}

func (l *auditLog) record(event SessionEvent) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail != nil && event.Kind == SessionStarted {
		return l.fail
	}
	l.events = append(l.events, event)
	return nil
}

func (l *auditLog) wait(t *testing.T, count int) []SessionEvent {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		if len(l.events) >= count {
			events := append([]SessionEvent(nil), l.events...)
			l.mu.Unlock()
			return events
		}
		l.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t.Fatalf("wanted %d audit events, have %+v", count, l.events)
	return nil
}

func auditedHost(t *testing.T, log *auditLog) *Host {
	t.Helper()
	listener := loopbackListener(t)
	t.Cleanup(func() { listener.Close() })
	go echoConnections(listener)
	host, err := NewHost(HostConfig{ProbeTimeout: time.Second, DialTimeout: time.Second, Audit: log.record})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.ReconcileTarget(context.Background(), 4, uint16(listener.Addr().(*net.TCPAddr).Port)); err != nil {
		t.Fatal(err)
	}
	return host
}

var auditPeer = Peer{AccountID: "user_a", EndpointID: "cli_a", OperationID: "op_1"}

// F5-12: a served session is audited from its start to its end, with the bytes each way and who it was.
func TestServeAuditsTheSessionStartAndEndWithBytes(t *testing.T) {
	log := &auditLog{}
	host := auditedHost(t, log)
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() { _, err := host.Serve(WithPeer(context.Background(), auditPeer), 4, server); done <- err }()
	message := []byte("SSH-2.0-audit\r\n")
	if _, err := client.Write(message); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(client, make([]byte, len(message))); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	<-done
	events := log.wait(t, 2)
	start, end := events[0], events[1]
	if start.Kind != SessionStarted || start.Peer != auditPeer || start.Generation != 4 || start.SessionID == "" {
		t.Fatalf("start = %+v", start)
	}
	if end.Kind != SessionEnded || end.SessionID != start.SessionID || end.Outcome != EndedClosed || end.BytesIn != int64(len(message)) || end.BytesOut != int64(len(message)) {
		t.Fatalf("end = %+v", end)
	}
}

// A refused stream names why: a stale or disabled target is not set up. There is no session limit of ours
// (the machine's sshd decides), so many concurrent streams are all served.
func TestServeAuditsRefusalsWithTheirReason(t *testing.T) {
	log := &auditLog{}
	host := auditedHost(t, log)
	ctx := WithPeer(context.Background(), auditPeer)
	extra, extraServer := net.Pipe()
	defer extra.Close()
	sent := make(chan error, 2)
	go func() {
		remote := bufio.NewReader(extra)
		for range 2 {
			sent <- ReadRefusal(remote)
		}
	}()
	if _, err := host.Serve(ctx, 9, extraServer); !errors.Is(err, ErrSSHHostStale) {
		t.Fatalf("stale: %v", err)
	}
	host.Refused(ctx, extraServer, 0, RefusedNotSetUp)
	// F5-09: the client reads each reason from the stream.
	for range 2 {
		var refused *RefusedError
		if err := <-sent; !errors.As(err, &refused) || refused.Reason != RefusedNotSetUp {
			t.Fatalf("client read %v, want %s", err, RefusedNotSetUp)
		}
	}
	for _, event := range log.wait(t, 2) {
		if event.Kind != SessionRefused || event.Reason != RefusedNotSetUp || event.Peer != auditPeer {
			t.Fatalf("refusal = %+v", event)
		}
	}
}

func TestServeHasNoSessionLimit(t *testing.T) {
	host := auditedHost(t, &auditLog{})
	ctx := WithPeer(context.Background(), auditPeer)
	const sessions = 40 // above the 32 the host used to cap at
	for range sessions {
		client, server := net.Pipe()
		t.Cleanup(func() { _ = client.Close() })
		go func() { _, _ = host.Serve(ctx, 4, server) }()
	}
	for deadline := time.Now().Add(5 * time.Second); host.OpenStreams() != sessions; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("open streams = %d, want %d", host.OpenStreams(), sessions)
		}
	}
}

// A session cut by Disable (access revoked) ends as revoked, not closed.
func TestServeAuditsARevokedSession(t *testing.T) {
	log := &auditLog{}
	host := auditedHost(t, log)
	client, server := net.Pipe()
	defer client.Close()
	go func() { _, _ = host.Serve(WithPeer(context.Background(), auditPeer), 4, server) }()
	for host.OpenStreams() != 1 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := host.Disable(ctx); err != nil {
		t.Fatal(err)
	}
	if end := log.wait(t, 2)[1]; end.Kind != SessionEnded || end.Outcome != EndedRevoked {
		t.Fatalf("end = %+v", end)
	}
}

// An offline session whose start cannot be recorded is refused (audit fails closed); an online one is served.
func TestServeRefusesAnOfflineSessionItCannotRecord(t *testing.T) {
	log := &auditLog{fail: errors.New("backlog full")}
	host := auditedHost(t, log)
	offline := auditPeer
	offline.Offline = true
	_, server := net.Pipe()
	if _, err := host.Serve(WithPeer(context.Background(), offline), 4, server); !errors.Is(err, ErrSSHAuditUnavailable) {
		t.Fatalf("offline with a full backlog: %v", err)
	}
	if host.OpenStreams() != 0 {
		t.Fatal("the refused offline session still holds a stream slot")
	}
	client, onlineServer := net.Pipe()
	done := make(chan error, 1)
	go func() { _, err := host.Serve(WithPeer(context.Background(), auditPeer), 4, onlineServer); done <- err }()
	_, _ = client.Write([]byte("x"))
	_, _ = io.ReadFull(client, make([]byte, 1))
	_ = client.Close()
	if err := <-done; errors.Is(err, ErrSSHAuditUnavailable) {
		t.Fatalf("an online session was refused because audit failed: %v", err)
	}
}

// A stream with no named peer is served and not audited.
func TestServeWithoutAPeerIsNotAudited(t *testing.T) {
	log := &auditLog{}
	host := auditedHost(t, log)
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() { _, err := host.Serve(context.Background(), 4, server); done <- err }()
	_ = client.Close()
	<-done
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.events) != 0 {
		t.Fatalf("events = %+v", log.events)
	}
}
