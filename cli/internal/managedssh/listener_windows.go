//go:build windows

package managedssh

import (
	"errors"
	"net"
	"strings"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func ListenOwnerSocket(path string) (listener net.Listener, resultErr error) {
	defer func() { resultErr = managedSSHBoundary("listener_bind", resultErr) }()
	if !validWindowsAgentPipe(path) {
		return nil, ErrAgentDenied
	}
	sid, err := currentManagedSSHSID()
	if err != nil {
		return nil, err
	}
	want, err := ownerAgentSocket("")
	if err != nil || !strings.EqualFold(path, want) {
		return nil, ErrAgentDenied
	}
	pipeListener, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: managedSSHPipeSDDL(sid), MessageMode: false, InputBufferSize: MaxAgentRequestBytes + 4, OutputBufferSize: MaxAgentRequestBytes + 4})
	if err != nil {
		if managedSSHAgentPipeConflict(err) {
			return nil, managedSSHFailure("listener_bind", ErrAgentDenied, err)
		}
		return nil, err
	}
	return pipeListener, nil
}

func managedSSHAgentPipeConflict(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_FILE_EXISTS) ||
		errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_PIPE_BUSY)
}
