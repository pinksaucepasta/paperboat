package managedssh

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// macOS: Remote Login can be limited to chosen users (com.apple.access_ssh); sshd then refuses the others after
// accepting their key, which the client saw only as "Connection closed".
func TestRemoteLoginDeniedOnlyWhenTheGroupExistsAndTheUserIsNotInIt(t *testing.T) {
	run := func(groupExists bool, member int, memberErr error) ExitRunner {
		return func(name string, args ...string) (int, error) {
			if name == "dscl" {
				if groupExists {
					return 0, nil
				}
				return 56, nil
			}
			return member, memberErr
		}
	}
	for _, test := range []struct {
		name   string
		user   string
		runner ExitRunner
		denied bool
	}{
		{"no group: every user may log in", "pbmac2", run(false, 67, nil), false},
		{"member of the group", "pbmac2", run(true, 0, nil), false},
		{"group exists, user not in it", "pbmac2", run(true, 67, nil), true},
		{"dseditgroup cannot run: sshd stays the authority", "pbmac2", run(true, 0, errors.New("not found")), false},
		{"no user", "", run(true, 67, nil), false},
		{"a user name is never an option", "-o", run(true, 67, nil), false},
		{"no runner", "pbmac2", nil, false},
	} {
		if got := RemoteLoginDenied(test.user, test.runner); got != test.denied {
			t.Errorf("%s: denied=%v want %v", test.name, got, test.denied)
		}
	}
}

func remoteLoginHost(t *testing.T, log *auditLog, checks map[string]int) *Host {
	t.Helper()
	listener := loopbackListener(t)
	t.Cleanup(func() { listener.Close() })
	go echoConnections(listener)
	var mu sync.Mutex
	host, err := NewHost(HostConfig{ProbeTimeout: time.Second, DialTimeout: time.Second, Audit: log.record,
		LoginDenied: func(user string) bool { mu.Lock(); checks[user]++; mu.Unlock(); return user == "pbmac2" }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.ReconcileTarget(context.Background(), 4, uint16(listener.Addr().(*net.TCPAddr).Port)); err != nil {
		t.Fatal(err)
	}
	return host
}

// The check is for the user the stream really logs in as: the machine's setup user may be excluded from Remote Login
// while another allowed user logs in, and sshd would let that one in.
func TestServeRefusesOnlyTheLoginUserTheMachinesRemoteLoginDenies(t *testing.T) {
	log := &auditLog{}
	checks := map[string]int{}
	host := remoteLoginHost(t, log, checks)
	client, server := net.Pipe()
	defer client.Close()
	read := make(chan error, 1)
	go func() { read <- ReadRefusal(bufio.NewReader(client)) }()
	denied := WithLoginUser(WithPeer(context.Background(), auditPeer), "pbmac2")
	if _, err := host.Serve(denied, 4, server); !errors.Is(err, ErrSSHLoginNotAllowed) {
		t.Fatalf("serve: %v", err)
	}
	var refused *RefusedError
	if err := <-read; !errors.As(err, &refused) || refused.Reason != RefusedLoginNotAllowed+":pbmac2" || !strings.Contains(err.Error(), "pbmac2 isn't allowed to log in over SSH") || !strings.Contains(err.Error(), "Remote Login") {
		t.Fatalf("client read %v", err)
	}
	if event := log.wait(t, 1)[0]; event.Kind != SessionRefused || event.Reason != RefusedLoginNotAllowed {
		t.Fatalf("audit = %+v", event)
	}
	// Another user, and a stream that names none, are served: sshd decides.
	for name, ctx := range map[string]context.Context{
		"allowed user": WithLoginUser(WithPeer(context.Background(), auditPeer), "bob"),
		"no user":      WithPeer(context.Background(), auditPeer),
	} {
		c, s := net.Pipe()
		done := make(chan error, 1)
		go func() { _, err := host.Serve(ctx, 4, s); done <- err }()
		banner := []byte("SSH-2.0-x\r\n")
		if _, err := c.Write(banner); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(c, make([]byte, len(banner))); err != nil {
			t.Fatalf("%s: the stream was not served: %v", name, err)
		}
		c.Close()
		if err := <-done; errors.Is(err, ErrSSHLoginNotAllowed) {
			t.Fatalf("%s was refused", name)
		}
	}
	if checks["pbmac2"] != 1 || checks["bob"] != 1 || len(checks) != 2 {
		t.Fatalf("the Remote Login lookup ran %v: once per user, never without one", checks)
	}
}
