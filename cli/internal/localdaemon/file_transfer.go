package localdaemon

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/admission"
	"io"
	"net"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

const maxNativeTransferLeases = 64

type daemonTransferLease struct {
	peer   localapi.Peer
	opener tunnel.DirectTransferStreamOpener
	closer io.Closer
	// unlimited restricts the lease's streams to a direct or self-hosted relay path to machineID.
	unlimited bool
	machineID string
}

type transferLeaseCloser struct {
	closer io.Closer
	cancel context.CancelFunc
	once   sync.Once
	err    error
}

func (c *transferLeaseCloser) Close() error {
	if c == nil {
		return nil
	}
	c.once.Do(func() {
		c.cancel()
		c.err = c.closer.Close()
	})
	return c.err
}

// FileTransferBroker keeps direct file carriers inside the daemon and gives a
// local process only a PID-bound capability for opening application streams.
type FileTransferBroker struct {
	tunnel nativeTransferPreparer
	now    func() time.Time

	mu     sync.Mutex
	closed bool
	leases map[string]daemonTransferLease
}

type nativeTransferPreparer interface {
	PrepareNativeFileTransfer(context.Context, resolver.ConnectInfo, string) (tunnel.DirectTransferStreamOpener, error)
	// PathEligibility reports the path the engine uses to the machine and a channel closed on its next change.
	PathEligibility(context.Context, string) (tailnet.PathKind, <-chan struct{})
}

func NewFileTransferBroker(peerTunnel nativeTransferPreparer) (*FileTransferBroker, error) {
	if peerTunnel == nil {
		return nil, ErrInvalidInventoryConfig
	}
	return &FileTransferBroker{tunnel: peerTunnel, now: time.Now, leases: make(map[string]daemonTransferLease)}, nil
}

func (b *FileTransferBroker) PrepareFileTransfer(ctx context.Context, peer localapi.Peer, request localapi.FileTransferRequest) (localapi.FileTransferResult, error) {
	if b == nil || ctx == nil || peer.PID <= 0 || request.Validate(b.now().UTC()) != nil {
		return localapi.FileTransferResult{}, ErrInvalidInventoryConfig
	}
	info := resolver.ConnectInfo{TargetKind: "machine", MachineID: request.MachineID, MachineGeneration: request.MachineGeneration, Terminal: &resolver.TerminalTarget{EnvironmentID: request.EnvironmentID, Auth: resolver.AuthTarget{Method: "bearer", Token: request.Credential, ExpiresAt: request.Deadline.UTC().Format(time.RFC3339Nano), ResourceID: request.AccessSessionID}}}
	staged, err := admission.Stage(ctx, time.Time{})
	if err != nil {
		return localapi.FileTransferResult{}, err
	}
	leaseCtx := staged.Context()
	opener, err := b.tunnel.PrepareNativeFileTransfer(leaseCtx, info, request.OperationID)
	if err != nil {
		staged.Abort()
		return localapi.FileTransferResult{}, err
	}
	lifetime, err := staged.Accept()
	if err != nil {
		staged.Abort()
		if closer, ok := opener.(io.Closer); ok {
			_ = closer.Close()
		}
		return localapi.FileTransferResult{}, err
	}
	cancelLease := lifetime.Close
	closer, ok := opener.(io.Closer)
	if !ok {
		cancelLease()
		return localapi.FileTransferResult{}, errors.New("native file transfer lease lacks ownership")
	}
	handle, err := newTransferHandle()
	if err != nil {
		cancelLease()
		_ = closer.Close()
		return localapi.FileTransferResult{}, err
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		cancelLease()
		_ = closer.Close()
		return localapi.FileTransferResult{}, net.ErrClosed
	}
	if len(b.leases) >= maxNativeTransferLeases {
		b.mu.Unlock()
		cancelLease()
		_ = closer.Close()
		return localapi.FileTransferResult{}, errors.New("native file transfer lease limit reached")
	}
	b.leases[handle] = daemonTransferLease{peer: peer, opener: opener, closer: &transferLeaseCloser{closer: closer, cancel: cancelLease}, unlimited: request.TransferMode == "unlimited", machineID: request.MachineID}
	b.mu.Unlock()
	return localapi.FileTransferResult{Handle: handle}, nil
}

func (b *FileTransferBroker) OpenFileTransferStream(ctx context.Context, peer localapi.Peer, handle string) (net.Conn, error) {
	b.mu.Lock()
	lease, ok := b.leases[handle]
	if ok && !samePeer(lease.peer, peer) {
		ok = false
	}
	b.mu.Unlock()
	if !ok {
		return nil, localapi.ErrPermission
	}
	if !lease.unlimited {
		return lease.opener.OpenTransferStream(ctx)
	}
	return b.openEligibleStream(ctx, lease)
}

// eligiblePath reports whether an Unlimited transfer may use this path: never a Paperboat-hosted relay.
func eligiblePath(kind tailnet.PathKind) bool {
	return kind == tailnet.PathDirect || kind == tailnet.PathSelfHostedRelay
}

// openEligibleStream opens an Unlimited stream only on a direct or self-hosted relay path, and closes it as
// soon as the path moves to a hosted relay or none (PLAN 3b File transfer modes). At most the bytes in flight
// when the path changed cross the new path; the sender resumes from the committed offset when a path returns.
func (b *FileTransferBroker) openEligibleStream(ctx context.Context, lease daemonTransferLease) (net.Conn, error) {
	watch, stopWatch := context.WithCancel(context.Background())
	kind, changed := b.tunnel.PathEligibility(watch, lease.machineID)
	if !eligiblePath(kind) {
		stopWatch()
		return nil, &localapi.NoEligiblePathError{Path: string(kind)}
	}
	stream, err := lease.opener.OpenTransferStream(ctx)
	if err != nil {
		stopWatch()
		return nil, err
	}
	guarded := &pathGuardedConn{Conn: stream, stop: stopWatch}
	go func() {
		for {
			select {
			case <-watch.Done():
				return
			case <-changed:
			}
			if watch.Err() != nil {
				return
			}
			kind, changed = b.tunnel.PathEligibility(watch, lease.machineID)
			if !eligiblePath(kind) {
				_ = guarded.Close()
				return
			}
		}
	}()
	return guarded, nil
}

// pathGuardedConn ends its path watch when the stream closes.
type pathGuardedConn struct {
	net.Conn
	stop context.CancelFunc
}

func (c *pathGuardedConn) Close() error {
	c.stop()
	return c.Conn.Close()
}

func (b *FileTransferBroker) ReleaseFileTransfer(peer localapi.Peer, handle string) error {
	b.mu.Lock()
	lease, ok := b.leases[handle]
	if ok && samePeer(lease.peer, peer) {
		delete(b.leases, handle)
	} else {
		ok = false
	}
	b.mu.Unlock()
	if !ok {
		return localapi.ErrPermission
	}
	return lease.closer.Close()
}

func (b *FileTransferBroker) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	leases := b.leases
	b.leases = make(map[string]daemonTransferLease)
	b.mu.Unlock()
	var result error
	for _, lease := range leases {
		result = errors.Join(result, lease.closer.Close())
	}
	return result
}

func samePeer(left, right localapi.Peer) bool {
	if left.SID != "" || right.SID != "" {
		return left.SID != "" && left.SID == right.SID && left.PID == right.PID
	}
	return left.UID == right.UID && left.GID == right.GID && left.PID == right.PID
}

func newTransferHandle() (string, error) {
	var value [24]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}
