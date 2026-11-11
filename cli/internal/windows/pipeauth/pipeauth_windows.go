//go:build windows

// Package pipeauth authenticates local Windows named-pipe servers before any
// application bytes are exchanged.
//
// A named pipe name is shared by all of its instances and a client is served by whichever instance waits. The
// first instance's security descriptor governs the rest: creating another instance needs
// FILE_CREATE_PIPE_INSTANCE, which write access grants. So the server on the other end is trusted if and only if
// a trusted account created the pipe (its owner: only administrators can set an owner other than themselves) and
// the DACL lets no one else write to it. Prior art: tailscale safesocket/pipe_windows.go
// checkPipeExclusiveToCurrentUser.
package pipeauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

var dialPipe = func(ctx context.Context, path string) (net.Conn, error) {
	return winio.DialPipeAccessImpLevel(ctx, path, windows.GENERIC_READ|windows.GENERIC_WRITE, winio.PipeImpLevelIdentification)
}

// connectedDescriptor reads the owner and DACL of the pipe conn is connected to. GENERIC_READ includes
// READ_CONTROL.
var connectedDescriptor = func(conn net.Conn) (*windows.SECURITY_DESCRIPTOR, error) {
	handle, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return nil, errors.New("named pipe connection has no Windows handle")
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(handle.Fd()), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, pipeAuthFailure{message: "read named pipe security descriptor", cause: err}
	}
	return descriptor, nil
}

// currentOwner is the user the calling thread acts as: an impersonating thread's, else the process's.
var currentOwner = func() (string, error) {
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		return "", pipeAuthFailure{message: "resolve current Windows user", cause: err}
	}
	if user == nil || user.User.Sid == nil {
		return "", errors.New("resolve current Windows user")
	}
	return user.User.Sid.String(), nil
}

var (
	systemSID         = wellKnownSID(windows.WinLocalSystemSid)
	administratorsSID = wellKnownSID(windows.WinBuiltinAdministratorsSid)
)

func wellKnownSID(kind windows.WELL_KNOWN_SID_TYPE) string {
	sid, err := windows.CreateWellKnownSid(kind)
	if err != nil {
		panic(err)
	}
	return sid.String()
}

// DialCurrentUser connects at identification impersonation level and rejects a pipe not created by the current
// process user, or one whose DACL lets anyone but that user, SYSTEM or Administrators serve it.
func DialCurrentUser(ctx context.Context, path string) (net.Conn, error) {
	expected, err := currentOwner()
	if err != nil {
		return nil, err
	}
	return dialChecked(ctx, path, func(descriptor *windows.SECURITY_DESCRIPTOR) error {
		return CheckOwnedBy(descriptor, expected)
	})
}

// DialSystemOwner permits only LocalSystem to probe a specifically bound user
// pipe. It still authenticates the connected object before exchanging bytes.
func DialSystemOwner(ctx context.Context, path, ownerSID string) (net.Conn, error) {
	caller, err := currentOwner()
	if err != nil {
		return nil, err
	}
	owner, sidErr := windows.StringToSid(ownerSID)
	if caller != systemSID || sidErr != nil || owner == nil || !owner.IsValid() {
		return nil, pipeAuthFailure{message: "owner-specific named pipe probe requires LocalSystem and a valid owner", cause: sidErr}
	}
	return dialChecked(ctx, path, func(descriptor *windows.SECURITY_DESCRIPTOR) error {
		return CheckOwnedBy(descriptor, owner.String())
	})
}

func dialChecked(ctx context.Context, path string, check func(*windows.SECURITY_DESCRIPTOR) error) (net.Conn, error) {
	conn, err := dialPipe(ctx, path)
	if err != nil {
		return nil, pipeAuthFailure{message: "connect named pipe", cause: err}
	}
	if err := VerifyConn(conn, check); err != nil {
		return nil, err
	}
	return conn, nil
}

// VerifyConn runs check on the security descriptor of the pipe conn is connected to and closes conn when the
// descriptor cannot be read or check fails.
func VerifyConn(conn net.Conn, check func(*windows.SECURITY_DESCRIPTOR) error) error {
	descriptor, err := connectedDescriptor(conn)
	if err == nil {
		err = check(descriptor)
	}
	if err != nil {
		return pipeAuthFailure{message: "refusing named pipe not owned by the expected account", cause: errors.Join(err, conn.Close())}
	}
	return nil
}

// CheckOwnedBy accepts a pipe owned by owner whose DACL grants access to no one but owner, SYSTEM and
// Administrators.
func CheckOwnedBy(descriptor *windows.SECURITY_DESCRIPTOR, owner string) error {
	return checkDescriptor(descriptor, []string{owner}, []string{owner, systemSID, administratorsSID}, false)
}

// CheckService accepts a pipe created by SYSTEM, Administrators, the current user self or owner (the enrolled
// account that runs the server, "" when there is none besides those). Its DACL must grant access to no one else
// (tailscale's exclusive check). SYSTEM's per-user pipes also grant their one enrolled user, who can therefore
// serve them as well: a pipe created by SYSTEM or Administrators may name one further account (never a group).
func CheckService(descriptor *windows.SECURITY_DESCRIPTOR, self, owner string) error {
	trusted := []string{systemSID, administratorsSID, self}
	if owner != "" {
		trusted = append(trusted, owner)
	}
	return checkDescriptor(descriptor, trusted, trusted, true)
}

func checkDescriptor(descriptor *windows.SECURITY_DESCRIPTOR, owners, servers []string, delegate bool) error {
	if descriptor == nil {
		return errors.New("named pipe has no security descriptor")
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.IsValid() {
		return errors.Join(errors.New("named pipe has no valid owner"), err)
	}
	if !slices.Contains(owners, owner.String()) {
		return fmt.Errorf("named pipe is owned by %s, not by a trusted account", owner)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return fmt.Errorf("read named pipe DACL: %w", err)
	}
	if dacl == nil {
		return errors.New("named pipe has no DACL, so any account may serve it")
	}
	extra := ""
	for index := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return fmt.Errorf("read named pipe DACL: %w", err)
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue // a deny ACE lets no one in
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		default:
			return fmt.Errorf("named pipe DACL has an unexpected entry type %d", ace.Header.AceType)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if slices.Contains(servers, sid) || sid == extra {
			continue
		}
		if delegate && extra == "" && accountSID(sid) && (owner.String() == systemSID || owner.String() == administratorsSID) {
			extra = sid
			continue
		}
		return fmt.Errorf("named pipe grants access to %s, so another account could serve it", sid)
	}
	return nil
}

// accountSID reports a local, domain or Microsoft Entra account (or domain group) SID, never a built-in or
// well-known group such as Users or Everyone, which would let any account serve the pipe.
func accountSID(sid string) bool {
	return strings.HasPrefix(sid, "S-1-5-21-") || strings.HasPrefix(sid, "S-1-12-1-")
}

type pipeAuthFailure struct {
	message string
	cause   error
}

func (e pipeAuthFailure) Error() string { return e.message }
func (e pipeAuthFailure) Unwrap() error { return e.cause }
