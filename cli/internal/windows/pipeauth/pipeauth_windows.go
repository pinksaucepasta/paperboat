//go:build windows

// Package pipeauth authenticates local Windows named-pipe servers before any
// application bytes are exchanged.
package pipeauth

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

var dialPipe = func(ctx context.Context, path string) (net.Conn, error) {
	return winio.DialPipeAccessImpLevel(ctx, path, windows.GENERIC_READ|windows.GENERIC_WRITE, winio.PipeImpLevelIdentification)
}

var connectedOwner = func(conn net.Conn) (string, error) {
	handle, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return "", errors.New("named pipe connection has no Windows handle")
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(handle.Fd()), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return "", fmt.Errorf("read named pipe owner: %w", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil {
		return "", errors.New("read named pipe owner")
	}
	return owner.String(), nil
}

var currentOwner = func() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return "", errors.New("resolve current Windows user")
	}
	return user.User.Sid.String(), nil
}

// DialCurrentUser connects at identification impersonation level and rejects
// a pipe object not owned by the current process user.
func DialCurrentUser(ctx context.Context, path string) (net.Conn, error) {
	expected, err := currentOwner()
	if err != nil {
		return nil, err
	}
	return dialExpectedOwner(ctx, path, expected)
}

// DialSystemOwner permits only LocalSystem to probe a specifically bound user
// pipe. It still authenticates the connected object before exchanging bytes.
func DialSystemOwner(ctx context.Context, path, ownerSID string) (net.Conn, error) {
	caller, err := currentOwner()
	if err != nil {
		return nil, err
	}
	owner, err := windows.StringToSid(ownerSID)
	if caller != "S-1-5-18" || err != nil || owner == nil || !owner.IsValid() {
		return nil, errors.New("owner-specific named pipe probe requires LocalSystem and a valid owner")
	}
	return dialExpectedOwner(ctx, path, ownerSID)
}

func dialExpectedOwner(ctx context.Context, path, expected string) (net.Conn, error) {
	conn, err := dialPipe(ctx, path)
	if err != nil {
		return nil, err
	}
	owner, ownerErr := connectedOwner(conn)
	if ownerErr != nil || owner == "" || expected == "" || owner != expected {
		_ = conn.Close()
		if ownerErr != nil {
			return nil, ownerErr
		}
		return nil, errors.New("refusing named pipe not owned by the expected account")
	}
	return conn, nil
}
