//go:build !linux && !darwin && !windows

package deviceguard

import (
	"context"
	"net"
	"net/netip"
)

type Client struct{}

func Connect(context.Context, string) (*Client, error) { return nil, ErrUnsupported }
func (*Client) Acquire(context.Context, string, netip.Addr, int) (net.Listener, error) {
	return nil, ErrUnsupported
}
func (*Client) ReplaceNames(context.Context, map[string]netip.Addr) error { return ErrUnsupported }
func (*Client) Status(context.Context) (RuntimeStatus, error)             { return RuntimeStatus{}, ErrUnsupported }
func (*Client) PrepareRangeChange(context.Context, string) error          { return ErrUnsupported }
func (*Client) Close() error                                              { return nil }
func Serve(context.Context, Config) error                                 { return ErrUnsupported }
func Install(context.Context, string, ...string) error                    { return ErrUnsupported }

const DefaultSocket = ""
const DefaultStateDir = ""

func (*Client) Certificate(context.Context, string) (CertificateBundle, error) {
	return CertificateBundle{}, ErrUnsupported
}

func (*Client) ReplaceAliases(context.Context, map[string]string) error { return ErrUnsupported }
