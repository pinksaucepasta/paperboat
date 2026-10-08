//go:build windows

package availability

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostservice"
	hostruntimeservice "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
)

func windowsOwnerHostServicePipe() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return "", ErrInvalid
	}
	instance, err := hostruntimeservice.WindowsUserInstance(user.User.Sid.String())
	if err != nil {
		return "", ErrInvalid
	}
	return hostservice.WindowsSocketPath(instance)
}

func dialAvailabilityHostService(ctx context.Context, path string, timeout time.Duration) (net.Conn, error) {
	canonical, err := windowsOwnerHostServicePipe()
	if err != nil || !strings.EqualFold(path, canonical) || timeout <= 0 {
		return nil, ErrInvalid
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return winio.DialPipeContext(dialCtx, canonical)
}

// Named pipes preserve message completion without a half-close. The server
// parses one bounded JSON request, then replies on the same duplex pipe.
func closeAvailabilityHostServiceWrite(net.Conn) error { return nil }

func NewHostClient(socketPath string, timeout time.Duration) (*HostClient, error) {
	canonical, err := windowsOwnerHostServicePipe()
	if err != nil || !strings.EqualFold(socketPath, canonical) || timeout <= 0 {
		return nil, ErrInvalid
	}
	return &HostClient{socketPath: canonical, timeout: timeout}, nil
}
