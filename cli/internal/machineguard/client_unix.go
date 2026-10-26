//go:build linux || darwin

package machineguard

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

type Client struct {
	// claims records whether the guard answered status with FeatureAddressClaims.
	claims atomic.Bool
	mu     sync.Mutex
	conn   *net.UnixConn
}

type peerIdentityError struct{ cause error }

func (e peerIdentityError) Error() string { return "machine guard is not owned by root" }
func (e peerIdentityError) Unwrap() error { return e.cause }

func Connect(ctx context.Context, path string) (*Client, error) {
	if path == "" {
		path = DefaultSocket
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	conn := connection.(*net.UnixConn)
	uid, err := peerUID(conn)
	if err != nil || uid != 0 {
		return nil, peerIdentityError{cause: errors.Join(err, conn.Close())}
	}
	return &Client{conn: conn}, nil
}
func writeFrame(writer io.Writer, data []byte) error {
	if len(data) > 60<<10 {
		return errors.New("machine guard request too large")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	if _, err := io.Copy(writer, bytes.NewReader(header[:])); err != nil {
		return err
	}
	_, err := io.Copy(writer, bytes.NewReader(data))
	return err
}

func readFrame(reader io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > 60<<10 {
		return nil, errors.New("invalid machine guard frame size")
	}
	data := make([]byte, int(size))
	_, err := io.ReadFull(reader, data)
	return data, err
}
func (c *Client) exchange(ctx context.Context, in request) (response, *os.File, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return response{}, nil, net.ErrClosed
	}
	connection := c.conn
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return response{}, nil, err
	}
	canceled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = connection.SetDeadline(time.Now()); close(canceled) })
	defer func() {
		if !stop() {
			<-canceled
		}
	}()
	var file *os.File
	fail := func(err error) (response, *os.File, error) {
		var closeErr error
		if file != nil {
			closeErr = errors.Join(closeErr, file.Close())
		}
		closeErr = errors.Join(closeErr, connection.Close())
		c.conn = nil
		return response{}, nil, errors.Join(err, closeErr)
	}
	data, err := json.Marshal(in)
	if err != nil {
		return response{}, nil, err
	}
	if err = writeFrame(connection, data); err != nil {
		return fail(err)
	}
	marker := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(4))
	n, on, flags, _, err := connection.ReadMsgUnix(marker, oob)
	if err != nil {
		return fail(err)
	}
	messages, err := unix.ParseSocketControlMessage(oob[:on])
	if err != nil {
		return fail(err)
	}
	for _, message := range messages {
		fds, e := unix.ParseUnixRights(&message)
		if e != nil {
			continue
		}
		for _, fd := range fds {
			unix.CloseOnExec(fd)
			if file == nil {
				file = os.NewFile(uintptr(fd), "paperboat-guard-listener")
			} else {
				unix.Close(fd)
			}
		}
	}
	if n != 1 || marker[0] != 0 || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
		return fail(errors.New("invalid machine guard descriptor marker"))
	}
	data, err = readFrame(connection)
	if err != nil {
		return fail(err)
	}
	var out response
	if err = json.Unmarshal(data, &out); err != nil {
		return fail(err)
	}
	if responseErr := responseError(out); responseErr != nil {
		if file != nil {
			responseErr = errors.Join(responseErr, file.Close())
		}
		return response{}, nil, responseErr
	}
	return out, file, nil
}
func (c *Client) Acquire(ctx context.Context, hostname string, ip netip.Addr, port int) (net.Listener, error) {
	listener, _, err := c.acquire(ctx, request{Operation: "acquire", Hostname: hostname, IP: ip.String(), Port: port})
	return listener, err
}

// AcquireMachine opens a listener for one machine of one account; the guard assigns the address (the preferred
// server address when free on this host) and returns it.
func (c *Client) AcquireMachine(ctx context.Context, account, machine, hostname string, preferred netip.Addr, port int) (net.Listener, netip.Addr, error) {
	in := request{Operation: "acquire", Hostname: hostname, IP: preferred.String(), Port: port}
	if c.claims.Load() {
		in.Account, in.Machine = account, machine
	}
	return c.acquire(ctx, in)
}

// ReplaceClaims sends the complete machine set of account from a verified configuration; the guard releases the
// rest into its hold.
func (c *Client) ReplaceClaims(ctx context.Context, account string, machines []string) error {
	if !c.claims.Load() {
		return nil // an older guard keeps no claims to release
	}
	_, _, err := c.exchange(ctx, request{Operation: "claims", Account: account, Machines: machines})
	return err
}

func (c *Client) acquire(ctx context.Context, in request) (net.Listener, netip.Addr, error) {
	out, file, err := c.exchange(ctx, in)
	if err != nil {
		return nil, netip.Addr{}, err
	}
	ip, port := netip.MustParseAddr(in.IP), in.Port
	if out.IP != "" {
		assigned, parseErr := netip.ParseAddr(out.IP)
		if parseErr != nil {
			if file != nil {
				file.Close()
			}
			return nil, netip.Addr{}, errors.New("machine guard returned an invalid address")
		}
		ip = assigned
	}
	if file == nil {
		return nil, netip.Addr{}, errors.New("machine guard returned no listener")
	}
	defer file.Close()
	listener, err := net.FileListener(file)
	if err != nil {
		_, _, _ = c.exchange(context.Background(), request{Operation: "release", IP: ip.String(), Port: port})
		return nil, netip.Addr{}, err
	}
	return &ownedListener{Listener: listener, client: c, ip: ip.String(), port: port}, ip, nil
}
func (c *Client) ReplaceAliases(ctx context.Context, aliases map[string]string) error {
	_, file, err := c.exchange(ctx, request{Operation: "aliases", Names: aliases})
	if file != nil {
		file.Close()
	}
	return err
}
func (c *Client) ReplaceNames(ctx context.Context, names map[string]netip.Addr) error {
	values := make(map[string]string, len(names))
	for name, ip := range names {
		values[name] = ip.String()
	}
	_, file, err := c.exchange(ctx, request{Operation: "names", Names: values})
	if file != nil {
		file.Close()
	}
	return err
}
func (c *Client) Status(ctx context.Context) (RuntimeStatus, error) {
	out, file, err := c.exchange(ctx, request{Operation: "status"})
	if file != nil {
		file.Close()
	}
	if err != nil {
		return RuntimeStatus{}, err
	}
	if out.Status == nil || !out.Status.Ready {
		return RuntimeStatus{}, errors.New("machine guard returned no ready status")
	}
	c.claims.Store(slices.Contains(out.Status.Features, FeatureAddressClaims))
	return *out.Status, nil
}
func (c *Client) PrepareRangeChange(ctx context.Context, cidr string) error {
	_, file, err := c.exchange(ctx, request{Operation: "prepare_range", LoopbackCIDR: cidr})
	if file != nil {
		file.Close()
	}
	return err
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

type ownedListener struct {
	net.Listener
	client *Client
	ip     string
	port   int
	once   sync.Once
	err    error
}

func (l *ownedListener) Close() error {
	l.once.Do(func() {
		_, _, err := l.client.exchange(context.Background(), request{Operation: "release", IP: l.ip, Port: l.port})
		l.err = errors.Join(err, l.Listener.Close())
	})
	return l.err
}

func (c *Client) Certificate(ctx context.Context, hostname string) (CertificateBundle, error) {
	out, file, err := c.exchange(ctx, request{Operation: "certificate", Hostname: hostname})
	if file != nil {
		file.Close()
	}
	if err != nil {
		return CertificateBundle{}, err
	}
	if out.Certificate == nil {
		return CertificateBundle{}, errors.New("machine guard returned no browser certificate")
	}
	return *out.Certificate, nil
}
