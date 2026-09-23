//go:build windows

package deviceguard

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
)

type Client struct {
	mu   sync.Mutex
	conn net.Conn
}

func Connect(ctx context.Context, path string) (*Client, error) {
	if path == "" {
		path = DefaultSocket
	}
	conn, err := winio.DialPipeAccessImpLevel(ctx, path, pipeClientAccess, winio.PipeImpLevelImpersonation)
	if err != nil {
		return nil, err
	}
	if err = verifySystemPipe(conn); err != nil {
		conn.Close()
		return nil, err
	}
	if err = writePipePreface(ctx, conn); err != nil {
		conn.Close()
		return nil, err
	}
	return &Client{conn: conn}, nil
}

func writePipePreface(ctx context.Context, conn net.Conn) error {
	deadline := time.Now().Add(pipeHandshakeTimeout)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	written, err := conn.Write(pipeClientPreface)
	if err == nil && written != len(pipeClientPreface) {
		err = io.ErrShortWrite
	}
	clearErr := conn.SetWriteDeadline(time.Time{})
	return errors.Join(err, clearErr)
}
func (c *Client) exchange(ctx context.Context, in request) (response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return response{}, net.ErrClosed
	}
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := c.conn.SetDeadline(deadline); err != nil {
		return response{}, err
	}
	conn := c.conn
	callbackDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()); close(callbackDone) })
	defer func() {
		if !stop() {
			<-callbackDone
		}
		if c.conn != nil {
			_ = c.conn.SetDeadline(time.Time{})
		}
	}()
	if err := writeFrame(c.conn, in); err != nil {
		c.conn.Close()
		c.conn = nil
		return response{}, err
	}
	var out response
	if err := readFrame(c.conn, &out); err != nil {
		c.conn.Close()
		c.conn = nil
		return out, err
	}
	if out.Error != "" {
		return out, errors.New(out.Error)
	}
	return out, nil
}
func (c *Client) Acquire(ctx context.Context, hostname string, ip netip.Addr, port int) (net.Listener, error) {
	out, err := c.exchange(ctx, request{Operation: "acquire", Hostname: hostname, IP: ip.String(), Port: port})
	if err != nil {
		return nil, err
	}
	if len(out.Socket) == 0 {
		return nil, errors.New("device guard returned no listener")
	}
	proxy := &windowsProxyListener{path: string(out.Socket), done: make(chan struct{})}
	return &windowsOwnedListener{windowsProxyListener: proxy, client: c, ip: ip.String(), port: port}, nil
}
func (c *Client) ReplaceAliases(ctx context.Context, aliases map[string]string) error {
	_, err := c.exchange(ctx, request{Operation: "aliases", Names: aliases})
	return err
}
func (c *Client) ReplaceNames(ctx context.Context, names map[string]netip.Addr) error {
	values := make(map[string]string, len(names))
	for name, ip := range names {
		values[name] = ip.String()
	}
	_, err := c.exchange(ctx, request{Operation: "names", Names: values})
	return err
}
func (c *Client) Status(ctx context.Context) (RuntimeStatus, error) {
	out, err := c.exchange(ctx, request{Operation: "status"})
	if err != nil {
		return RuntimeStatus{}, err
	}
	if out.Status == nil || !out.Status.Ready {
		return RuntimeStatus{}, errors.New("device guard returned no ready status")
	}
	return *out.Status, nil
}
func (c *Client) PrepareRangeChange(ctx context.Context, cidr string) error {
	_, err := c.exchange(ctx, request{Operation: "prepare_range", LoopbackCIDR: cidr})
	return err
}
func (c *Client) Certificate(ctx context.Context, hostname string) (CertificateBundle, error) {
	out, err := c.exchange(ctx, request{Operation: "certificate", Hostname: hostname})
	if err != nil {
		return CertificateBundle{}, err
	}
	if out.Certificate == nil {
		return CertificateBundle{}, errors.New("device guard returned no certificate")
	}
	return *out.Certificate, nil
}
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

type windowsOwnedListener struct {
	*windowsProxyListener
	client *Client
	ip     string
	port   int
	once   sync.Once
	err    error
}

func (l *windowsOwnedListener) Close() error {
	l.once.Do(func() {
		_, releaseErr := l.client.exchange(context.Background(), request{Operation: "release", IP: l.ip, Port: l.port})
		l.err = errors.Join(releaseErr, l.windowsProxyListener.Close())
	})
	return l.err
}
