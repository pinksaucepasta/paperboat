//go:build windows

package pipeauth

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

type trackedPipe struct {
	closed   bool
	closeErr error
}

func (p *trackedPipe) Read([]byte) (int, error)         { return 0, net.ErrClosed }
func (p *trackedPipe) Write(b []byte) (int, error)      { return len(b), nil }
func (p *trackedPipe) Close() error                     { p.closed = true; return p.closeErr }
func (p *trackedPipe) LocalAddr() net.Addr              { return nil }
func (p *trackedPipe) RemoteAddr() net.Addr             { return nil }
func (p *trackedPipe) SetDeadline(time.Time) error      { return nil }
func (p *trackedPipe) SetReadDeadline(time.Time) error  { return nil }
func (p *trackedPipe) SetWriteDeadline(time.Time) error { return nil }

const (
	testUser  = "S-1-5-21-1-2-3-1001"
	testOther = "S-1-5-21-1-2-3-1002"
)

func descriptor(t *testing.T, sddl string) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	parsed, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatalf("parse %s: %v", sddl, err)
	}
	return parsed
}

func stubDial(t *testing.T, sddl string, readErr error) *trackedPipe {
	originalDial, originalDescriptor, originalOwner := dialPipe, connectedDescriptor, currentOwner
	t.Cleanup(func() { dialPipe, connectedDescriptor, currentOwner = originalDial, originalDescriptor, originalOwner })
	pipe := &trackedPipe{}
	dialPipe = func(context.Context, string) (net.Conn, error) { return pipe, nil }
	connectedDescriptor = func(net.Conn) (*windows.SECURITY_DESCRIPTOR, error) {
		if readErr != nil {
			return nil, readErr
		}
		return descriptor(t, sddl), nil
	}
	currentOwner = func() (string, error) { return testUser, nil }
	return pipe
}

func TestDialCurrentUserAcceptsOnlyAnExclusivePipeOfTheUser(t *testing.T) {
	for _, test := range []struct {
		name, sddl string
		accept     bool
	}{
		{"own exclusive pipe", "O:" + testUser + "D:P(A;;GA;;;SY)(A;;GA;;;" + testUser + ")", true},
		{"SYSTEM-owned impostor", "O:SYD:P(A;;GA;;;SY)(A;;GA;;;" + testUser + ")", false},
		{"own pipe another user may serve", "O:" + testUser + "D:P(A;;GA;;;SY)(A;;GA;;;" + testUser + ")(A;;GRGW;;;" + testOther + ")", false},
		{"own pipe every user may serve", "O:" + testUser + "D:P(A;;GA;;;" + testUser + ")(A;;GRGW;;;BU)", false},
		{"own pipe with no DACL", "O:" + testUser + "D:NO_ACCESS_CONTROL", false},
		{"deny entries are harmless", "O:" + testUser + "D:P(D;;GA;;;" + testOther + ")(A;;GA;;;" + testUser + ")", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pipe := stubDial(t, test.sddl, nil)
			conn, err := DialCurrentUser(context.Background(), `\\.\pipe\test`)
			if (err == nil) != test.accept || pipe.closed == test.accept {
				t.Fatalf("conn=%v err=%v closed=%v, want accept=%v", conn, err, pipe.closed, test.accept)
			}
		})
	}
}

func TestDialSystemOwnerRequiresLocalSystemAndTheOwnersPipe(t *testing.T) {
	pipe := stubDial(t, "O:"+testUser+"D:P(A;;GA;;;SY)(A;;GA;;;"+testUser+")", nil)
	if _, err := DialSystemOwner(context.Background(), `\\.\pipe\test`, testUser); err == nil {
		t.Fatal("a non-SYSTEM caller probed an owner pipe")
	}
	currentOwner = func() (string, error) { return systemSID, nil }
	if _, err := DialSystemOwner(context.Background(), `\\.\pipe\test`, testUser); err != nil || pipe.closed {
		t.Fatalf("SYSTEM probe of the owner's pipe: err=%v closed=%v", err, pipe.closed)
	}
	if _, err := DialSystemOwner(context.Background(), `\\.\pipe\test`, testOther); err == nil || !pipe.closed {
		t.Fatalf("SYSTEM accepted another user's pipe: err=%v closed=%v", err, pipe.closed)
	}
}

func TestCheckServiceRefusesPipesAnotherAccountCouldServe(t *testing.T) {
	for _, test := range []struct {
		name, sddl, self, owner string
		accept                  bool
	}{
		{"SYSTEM service pipe for the user", "O:SYD:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + testUser + ")", testUser, "", true},
		{"SYSTEM client of the enrolled user's SYSTEM pipe", "O:SYD:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + testUser + ")", systemSID, "", true},
		{"Administrators-owned service pipe", "O:BAD:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + testUser + ")", testUser, "", true},
		{"SYSTEM pipe granting two accounts", "O:SYD:P(A;;GA;;;SY)(A;;GA;;;" + testUser + ")(A;;GA;;;" + testOther + ")", systemSID, "", false},
		{"SYSTEM pipe granting every user", "O:SYD:P(A;;GA;;;SY)(A;;GRGW;;;BU)", testUser, "", false},
		{"SYSTEM pipe granting everyone", "O:SYD:P(A;;GA;;;SY)(A;;GRGW;;;WD)", systemSID, "", false},
		{"Entra account enrolled", "O:SYD:P(A;;GA;;;SY)(A;;GA;;;S-1-12-1-1-2-3-4)", systemSID, "", true},
		{"squatted by another user", "O:" + testOther + "D:P(A;;GA;;;SY)(A;;GA;;;" + testOther + ")", testUser, "", false},
		{"squatted with a matching DACL", "O:" + testOther + "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + testUser + ")", testUser, "", false},
		{"user carrier dialed by SYSTEM", "O:" + testUser + "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + testUser + ")", systemSID, testUser, true},
		{"another user's carrier dialed by SYSTEM", "O:" + testOther + "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + testOther + ")", systemSID, testUser, false},
		{"user pipe delegating to another user", "O:" + testUser + "D:P(A;;GA;;;" + testUser + ")(A;;GA;;;" + testOther + ")", testUser, "", false},
		{"no DACL", "O:SYD:NO_ACCESS_CONTROL", testUser, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := CheckService(descriptor(t, test.sddl), test.self, test.owner)
			if (err == nil) != test.accept {
				t.Fatalf("CheckService(%s) = %v, want accept=%v", test.sddl, err, test.accept)
			}
		})
	}
}

func TestRejectedPipeRetainsDescriptorAndCloseCausesSafely(t *testing.T) {
	readCause := errors.New("private descriptor lookup detail")
	closeCause := errors.New("private close detail")
	pipe := stubDial(t, "", readCause)
	pipe.closeErr = closeCause
	if conn, err := DialCurrentUser(context.Background(), `\\.\pipe\test`); conn != nil || err == nil || !pipe.closed {
		t.Fatalf("conn=%v err=%v closed=%v", conn, err, pipe.closed)
	} else {
		if !errors.Is(err, readCause) || !errors.Is(err, closeCause) {
			t.Fatalf("rejection lost descriptor or close cause: %v", err)
		}
		if strings.Contains(err.Error(), "private") {
			t.Fatalf("rejection exposed private cause text: %v", err)
		}
	}
}

const (
	nativeForeignPipe = `\\.\pipe\paperboat-daemonrpc-owner-review`
	nativeReadyFile   = `C:\ProgramData\paperboat-daemonrpc-owner-review.ready`
)

func TestSystemOwnedPipeFixture(t *testing.T) {
	owner, err := currentOwner()
	if err != nil {
		t.Fatal(err)
	}
	if owner != "S-1-5-18" {
		t.Skip("fixture requires LocalSystem")
	}
	listener, err := winio.ListenPipe(nativeForeignPipe, &winio.PipeConfig{SecurityDescriptor: "O:SYD:P(A;;GRGW;;;AU)(A;;GA;;;SY)"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err = os.WriteFile(nativeReadyFile, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(nativeReadyFile)
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
}

func TestNativeDialRejectsSystemOwnedImpostor(t *testing.T) {
	if os.Getenv("PAPERBOAT_DAEMONRPC_PRIVILEGED_TEST") != "1" {
		t.Skip("privileged native fixture is not enabled")
	}
	conn, err := DialCurrentUser(context.Background(), nativeForeignPipe)
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatal("accepted LocalSystem-owned impostor pipe for current user")
	}
}

