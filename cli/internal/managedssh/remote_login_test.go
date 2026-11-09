package managedssh

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
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

func TestServeRefusesAUserTheMachinesRemoteLoginDeniesAndAuditsTheBareReason(t *testing.T) {
	log := &auditLog{}
	listener := loopbackListener(t)
	t.Cleanup(func() { listener.Close() })
	go echoConnections(listener)
	checks := 0
	host, err := NewHost(HostConfig{ProbeTimeout: time.Second, DialTimeout: time.Second, Audit: log.record, LoginUser: "pbmac2",
		LoginDenied: func(user string) bool { checks++; return user == "pbmac2" }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.ReconcileTarget(context.Background(), 4, uint16(listener.Addr().(*net.TCPAddr).Port)); err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	defer client.Close()
	read := make(chan error, 1)
	go func() { read <- ReadRefusal(bufio.NewReader(client)) }()
	if _, err := host.Serve(WithPeer(context.Background(), auditPeer), 4, server); !errors.Is(err, ErrSSHLoginNotAllowed) {
		t.Fatalf("serve: %v", err)
	}
	var refused *RefusedError
	if err := <-read; !errors.As(err, &refused) || refused.Reason != RefusedLoginNotAllowed+":pbmac2" || !strings.Contains(err.Error(), "pbmac2 isn't allowed to log in over SSH") || !strings.Contains(err.Error(), "Remote Login") {
		t.Fatalf("client read %v", err)
	}
	if event := log.wait(t, 1)[0]; event.Kind != SessionRefused || event.Reason != RefusedLoginNotAllowed {
		t.Fatalf("audit = %+v", event)
	}
	// Every stream asks, the macOS lookup runs once per cache window.
	for range 3 {
		c, s := net.Pipe()
		go func() { _ = ReadRefusal(bufio.NewReader(c)) }()
		_, _ = host.Serve(WithPeer(context.Background(), auditPeer), 4, s)
		c.Close()
	}
	if checks != 1 {
		t.Fatalf("the Remote Login lookup ran %d times for four streams", checks)
	}
}
