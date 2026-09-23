//go:build linux || darwin

package deviceguard

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
	"sync"
	"time"
)

type Client struct {
	mu   sync.Mutex
	conn *net.UnixConn
}

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
		conn.Close()
		return nil, errors.New("device guard is not owned by root")
	}
	return &Client{conn: conn}, nil
}
func writeFrame(writer io.Writer, data []byte) error {
	if len(data) > 60<<10 {
		return errors.New("device guard request too large")
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
		return nil, errors.New("invalid device guard frame size")
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
		if file != nil {
			file.Close()
		}
		connection.Close()
		c.conn = nil
		return response{}, nil, err
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
		return fail(errors.New("invalid device guard descriptor marker"))
	}
	data, err = readFrame(connection)
	if err != nil {
		return fail(err)
	}
	var out response
	if err = json.Unmarshal(data, &out); err != nil {
		return fail(err)
	}
	if out.Error != "" {
		if file != nil {
			file.Close()
		}
		return response{}, nil, errors.New(out.Error)
	}
	return out, file, nil
}
func (c *Client) Acquire(ctx context.Context, hostname string, ip netip.Addr, port int) (net.Listener, error) {
	_, file, err := c.exchange(ctx, request{Operation: "acquire", Hostname: hostname, IP: ip.String(), Port: port})
	if err != nil {
		return nil, err
	}
	if file == nil {
		return nil, errors.New("device guard returned no listener")
	}
	defer file.Close()
	listener, err := net.FileListener(file)
	if err != nil {
		_, _, _ = c.exchange(context.Background(), request{Operation: "release", IP: ip.String(), Port: port})
		return nil, err
	}
	return &ownedListener{Listener: listener, client: c, ip: ip.String(), port: port}, nil
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
		return RuntimeStatus{}, errors.New("device guard returned no ready status")
	}
	return *out.Status, nil
}
func (c *Client) PrepareRangeChange(ctx context.Context, cidr string) error {
	_, file, err := c.exchange(ctx, request{Operation: "prepare_range", LoopbackCIDR: cidr})
	if file != nil {
		file.Close()
	}
	return err
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
