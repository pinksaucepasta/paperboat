//go:build windows

package hostdproto

import (
	"context"
	"github.com/Microsoft/go-winio"
	"github.com/pinksaucepasta/paperboat/internal/winpipe"
	"golang.org/x/sys/windows"
	"net"
)

func ListenWorkloads(path string) (net.Listener, error) {
	if !validPipePath(path) {
		return nil, ErrSocketConfig
	}
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: pipeSecurityDescriptor(user.User.Sid.String()), MessageMode: false, InputBufferSize: 64 << 10, OutputBufferSize: 64 << 10})
}
// DialWorkloads dials a workload pipe served by SYSTEM, the caller or ownerSID (winpipe.Dial).
func DialWorkloads(ctx context.Context, path, ownerSID string) (net.Conn, error) {
	if !validPipePath(path) {
		return nil, ErrSocketConfig
	}
	return winpipe.Dial(ctx, path, ownerSID)
}
func RemoveWorkloadSocket(string) error { return nil }
