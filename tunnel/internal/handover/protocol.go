package handover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
)

// The exchange, one JSON message per unixpacket datagram:
//
//	new → old  {"op":"hello"}
//	old → new  {"op":"sockets","epoch":…,"keys":[…],"more":…}  + the sockets (SCM_RIGHTS), in batches
//	old → new  {"op":"certificates","certs":[…],"more":…}  the active TLS certificates, in batches,
//	           while the old process still accepts (the new one imports them before it starts)
//	new → old  {"op":"ready"}      the new process is warm: certificates, admissions and routes loaded
//	old → new  {"op":"released"}   the old process stopped accepting; the new one accepts from now on
//
// The old process then drains and exits; its end of the connection closing tells the new process
// it may read UDP. A new process that disconnects before "ready" leaves the old one serving.

// FileName is the handover socket's name in the state directory both edge containers mount.
const FileName = "handover.sock"

const (
	maxMessage     = 256 << 10
	certBatchBytes = 128 << 10 // a certificate batch stays under maxMessage once base64-encoded
	socketsPerSend = 200       // below the kernel's SCM_MAX_FD (253)
	helloTimeout   = 5 * time.Second
)

var ErrHandover = errors.New("edge socket handover failed")

type message struct {
	Op    string   `json:"op"`
	Epoch string   `json:"epoch,omitempty"`
	Keys  []string `json:"keys,omitempty"`
	More  bool     `json:"more,omitempty"`
	// Certs are opaque certificate records (private keys inside) for the replacement; see tlscert.
	Certs [][]byte `json:"certs,omitempty"`
}

func send(conn *net.UnixConn, m message, files []*os.File) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	var rights []byte
	if len(files) != 0 {
		fds := make([]int, len(files))
		for i, f := range files {
			fds[i] = int(f.Fd())
		}
		rights = syscall.UnixRights(fds...)
	}
	_, _, err = conn.WriteMsgUnix(b, rights, nil)
	wipeRecords([][]byte{b}) // the message may carry certificate private keys
	return err
}

func receive(conn *net.UnixConn) (message, []*os.File, error) {
	b, oob := make([]byte, maxMessage), make([]byte, syscall.CmsgSpace(socketsPerSend*4))
	n, oobn, flags, _, err := conn.ReadMsgUnix(b, oob)
	if err != nil {
		return message{}, nil, err
	}
	var files []*os.File
	if oobn != 0 {
		controls, err := syscall.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			return message{}, nil, err
		}
		for _, control := range controls {
			fds, err := syscall.ParseUnixRights(&control)
			if err != nil {
				return message{}, nil, err
			}
			for _, fd := range fds {
				files = append(files, os.NewFile(uintptr(fd), "inherited"))
			}
		}
	}
	var m message
	defer wipeRecords([][]byte{b[:n]})
	if flags&(syscall.MSG_TRUNC|syscall.MSG_CTRUNC) != 0 || json.Unmarshal(b[:n], &m) != nil {
		closeAll(files)
		return message{}, nil, fmt.Errorf("%w: malformed message", ErrHandover)
	}
	return m, files, nil
}

func closeAll(files []*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// Inheritance is a replacement process's side of a handover in progress.
type Inheritance struct {
	Epoch string
	// Certificates are the serving process's active certificate records; the caller imports and
	// wipes them (tlscert.Registry.ImportEncoded).
	Certificates [][]byte
	conn         *net.UnixConn
	udp          chan struct{}
}

// Inherit takes over the sockets and process epoch of the edge process serving in stateDir. It
// returns nil when no process serves there (a fresh start). From then on Listen and ListenPacket
// return the inherited sockets, and nothing accepts until Ready.
func Inherit(stateDir string) (*Inheritance, error) {
	conn, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: filepath.Join(stateDir, FileName), Net: "unixpacket"})
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: connect to the serving edge process: %w", ErrHandover, err)
	}
	inheritance, err := inherit(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return inheritance, nil
}

func inherit(conn *net.UnixConn) (*Inheritance, error) {
	if err := requirePeerUID(conn, os.Geteuid()); err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(helloTimeout))
	if err := send(conn, message{Op: "hello"}, nil); err != nil {
		return nil, fmt.Errorf("%w: send hello: %w", ErrHandover, err)
	}
	inherited := map[string]*os.File{}
	epoch := ""
	for {
		m, files, err := receive(conn)
		if err != nil {
			closeAll(mapFiles(inherited))
			return nil, fmt.Errorf("%w: receive sockets: %w", ErrHandover, err)
		}
		if m.Op != "sockets" || m.Epoch == "" || len(m.Keys) != len(files) || epoch != "" && m.Epoch != epoch {
			closeAll(files)
			closeAll(mapFiles(inherited))
			return nil, fmt.Errorf("%w: unexpected %q message", ErrHandover, m.Op)
		}
		epoch = m.Epoch
		for i, k := range m.Keys {
			inherited[k] = files[i]
		}
		if !m.More {
			break
		}
	}
	var certs [][]byte
	for more := true; more; {
		m, files, err := receive(conn)
		closeAll(files)
		if err != nil || m.Op != "certificates" {
			closeAll(mapFiles(inherited))
			wipeRecords(certs)
			wipeRecords(m.Certs)
			return nil, fmt.Errorf("%w: receive certificates: unexpected %q message: %v", ErrHandover, m.Op, err)
		}
		certs = append(certs, m.Certs...)
		more = m.More
	}
	_ = conn.SetDeadline(time.Time{})
	s := &sockets{inherited: inherited, owned: map[string]*os.File{}, tcp: map[*listener]struct{}{}, accept: make(chan struct{}), udp: make(chan struct{}), released: make(chan struct{})}
	current = s
	return &Inheritance{Epoch: epoch, Certificates: certs, conn: conn, udp: s.udp}, nil
}

func mapFiles(m map[string]*os.File) []*os.File {
	files := make([]*os.File, 0, len(m))
	for _, f := range m {
		files = append(files, f)
	}
	return files
}

// Ready tells the old process this one is warm and waits until it stopped accepting; from then
// on this process accepts. Inherited sockets this process did not listen on are closed. UDPReady
// closes once the old process has exited.
func (i *Inheritance) Ready(ctx context.Context) error {
	if deadline, ok := ctx.Deadline(); ok {
		_ = i.conn.SetDeadline(deadline)
	}
	if err := send(i.conn, message{Op: "ready"}, nil); err != nil {
		return fmt.Errorf("%w: send ready: %w", ErrHandover, err)
	}
	m, files, err := receive(i.conn)
	closeAll(files)
	if err != nil || m.Op != "released" {
		return fmt.Errorf("%w: the serving process did not release its sockets: %v", ErrHandover, err)
	}
	_ = i.conn.SetDeadline(time.Time{})
	tookOverAt.Store(time.Now().UnixNano())
	s := current
	s.mu.Lock()
	close(s.accept)
	unused := mapFiles(s.inherited)
	s.inherited = map[string]*os.File{}
	s.mu.Unlock()
	closeAll(unused)
	go func() {
		// The old process's end closes when it exits; until then it alone reads UDP.
		_, _, _ = receive(i.conn)
		_ = i.conn.Close()
		close(i.udp)
	}()
	return nil
}

// Abort ends a handover before Ready: the old process keeps serving.
func (i *Inheritance) Abort() { _ = i.conn.Close() }

// Offer serves the handover socket in stateDir for this process's replacement. Once a replacement
// is ready, this process stops accepting, answers "released" and calls onRelease, which starts
// its drain. Offer returns a stop function that closes the socket.
func Offer(stateDir, epoch string, certificates func() ([][]byte, error), onRelease func()) (func(), error) {
	path := filepath.Join(stateDir, FileName)
	// A previous process's socket file is stale once this process serves: replace it.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: remove stale socket: %w", ErrHandover, err)
	}
	l, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		return nil, fmt.Errorf("%w: listen: %w", ErrHandover, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("%w: restrict socket: %w", ErrHandover, err)
	}
	l.SetUnlinkOnClose(false) // the replacement owns the path once it serves
	go func() {
		for {
			conn, err := l.AcceptUnix()
			if err != nil {
				return
			}
			if offer(conn, epoch, certificates) {
				_ = l.Close()
				onRelease()
				// Held open until this process exits: its closing tells the replacement to read UDP.
				return
			}
			_ = conn.Close()
		}
	}()
	return func() { _ = l.Close() }, nil
}

// offer runs one exchange; true once this process released its sockets to a ready replacement.
func offer(conn *net.UnixConn, epoch string, certificates func() ([][]byte, error)) bool {
	if requirePeerUID(conn, peerUID()) != nil {
		return false
	}
	_ = conn.SetDeadline(time.Now().Add(helloTimeout))
	if m, files, err := receive(conn); err != nil || m.Op != "hello" {
		closeAll(files)
		return false
	}
	keys, files := current.snapshot()
	for start := 0; start == 0 || start < len(keys); start += socketsPerSend {
		end := min(start+socketsPerSend, len(keys))
		if err := send(conn, message{Op: "sockets", Epoch: epoch, Keys: keys[start:end], More: end < len(keys)}, files[start:end]); err != nil {
			return false
		}
	}
	if !sendCertificates(conn, certificates) {
		return false
	}
	// The replacement warms up for as long as its own startup allows; an install bounds that.
	_ = conn.SetDeadline(time.Time{})
	if m, files, err := receive(conn); err != nil || m.Op != "ready" {
		closeAll(files)
		return false
	}
	current.release()
	// Released either way: this process no longer accepts, so it drains even if the reply is lost.
	_ = send(conn, message{Op: "released"}, nil)
	held = conn
	return true
}

// held keeps the released exchange's connection open (and out of the collector's reach) until
// this process exits.
var held *net.UnixConn

// peerUID is the user a handover peer must be; a variable so a test can name another one.
var peerUID = os.Geteuid

// sendCertificates sends the serving process's certificate records in batches, always ending with a
// message that has no More, and wipes them. A record too large for one message is skipped: the
// replacement then lacks that certificate and the server's next action for it repairs that.
func sendCertificates(conn *net.UnixConn, certificates func() ([][]byte, error)) bool {
	var records [][]byte
	if certificates != nil {
		var err error
		if records, err = certificates(); err != nil {
			return false
		}
	}
	defer wipeRecords(records)
	var batch [][]byte
	size := 0
	for _, record := range records {
		if len(record) > certBatchBytes {
			continue
		}
		if size+len(record) > certBatchBytes {
			if send(conn, message{Op: "certificates", Certs: batch, More: true}, nil) != nil {
				return false
			}
			batch, size = nil, 0
		}
		batch, size = append(batch, record), size+len(record)
	}
	return send(conn, message{Op: "certificates", Certs: batch}, nil) == nil
}

func wipeRecords(records [][]byte) {
	for _, record := range records {
		for i := range record {
			record[i] = 0
		}
	}
}

var tookOverAt atomic.Int64

// RemainingAfterTakeover is how much of window is left since this process took over accepting, or 0 when it
// did not take over one or the window passed. The window is the replaced process's drain deadline: hosts
// re-attach their carriers here on its GOAWAY, and nothing re-attaches after it.
func RemainingAfterTakeover(window time.Duration) time.Duration {
	if since, ok := SinceTakeover(); ok && since < window {
		return window - since
	}
	return 0
}

// SinceTakeover is how long ago this process took over accepting from the process it replaced, and
// false when it did not take over one.
func SinceTakeover() (time.Duration, bool) {
	at := tookOverAt.Load()
	if at == 0 {
		return 0, false
	}
	return time.Since(time.Unix(0, at)), true
}
