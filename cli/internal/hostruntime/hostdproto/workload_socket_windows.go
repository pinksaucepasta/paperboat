//go:build windows

package hostdproto

import (
	"context"
	"github.com/Microsoft/go-winio"
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
func DialWorkloads(ctx context.Context, path string) (net.Conn, error) {
	if !validPipePath(path) {
		return nil, ErrSocketConfig
	}
	return winio.DialPipeContext(ctx, path)
}
func RemoveWorkloadSocket(string) error { return nil }
