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
	conn, err := dialPipe(ctx, path)
	if err != nil {
		return nil, err
	}
	owner, ownerErr := connectedOwner(conn)
	expected, expectedErr := currentOwner()
	if ownerErr != nil || expectedErr != nil || owner == "" || expected == "" || owner != expected {
		_ = conn.Close()
		if ownerErr != nil {
			return nil, ownerErr
		}
		if expectedErr != nil {
			return nil, expectedErr
		}
		return nil, errors.New("refusing named pipe not owned by the current user")
	}
	return conn, nil
}
