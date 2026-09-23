package localdaemon

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

const maxNativeTransferLeases = 64

type daemonTransferLease struct {
	peer    localapi.Peer
	expires time.Time
	opener  tunnel.DirectTransferStreamOpener
	closer  io.Closer
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
	info := resolver.ConnectInfo{TargetKind: "machine", ProjectID: request.MachineID, MachineGeneration: request.MachineGeneration, Terminal: &resolver.TerminalTarget{EnvironmentID: request.EnvironmentID, Auth: resolver.AuthTarget{Method: "bearer", Token: request.Credential, ExpiresAt: request.Deadline.UTC().Format(time.RFC3339Nano), ResourceID: request.AccessSessionID}}}
	leaseCtx, cancelLease := context.WithCancel(context.Background())
	stopSetupCancel := context.AfterFunc(ctx, cancelLease)
	opener, err := b.tunnel.PrepareNativeFileTransfer(leaseCtx, info, request.OperationID)
	if err != nil {
		stopSetupCancel()
		cancelLease()
		return localapi.FileTransferResult{}, err
	}
	if !stopSetupCancel() || ctx.Err() != nil || leaseCtx.Err() != nil {
		cancelLease()
		if closer, ok := opener.(io.Closer); ok {
			_ = closer.Close()
		}
		return localapi.FileTransferResult{}, context.Canceled
	}
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
	b.leases[handle] = daemonTransferLease{peer: peer, expires: request.Deadline, opener: opener, closer: &transferLeaseCloser{closer: closer, cancel: cancelLease}}
	b.mu.Unlock()
	return localapi.FileTransferResult{Handle: handle}, nil
}

func (b *FileTransferBroker) OpenFileTransferStream(ctx context.Context, peer localapi.Peer, handle string) (net.Conn, error) {
	b.mu.Lock()
	lease, ok := b.leases[handle]
	expired := ok && !lease.expires.After(b.now().UTC())
	if expired {
		delete(b.leases, handle)
	}
	if ok && (!samePeer(lease.peer, peer) || expired) {
		ok = false
	}
	b.mu.Unlock()
	if expired {
		_ = lease.closer.Close()
	}
	if !ok {
		return nil, localapi.ErrPermission
	}
	return lease.opener.OpenTransferStream(ctx)
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
