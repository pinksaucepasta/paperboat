// Package controlsync consumes authorized full control-plane snapshots. It owns
// connection recovery, never admission or machine identity.
package controlsync

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/pinksaucepasta/paperboat/internal/api"
	pb "github.com/pinksaucepasta/paperboat/internal/controlsync/proto"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const ConnectTimeout = 5 * time.Second
const SnapshotTimeout = 15 * time.Second

type ClientConfig struct {
	ServerAddr   string
	Insecure     bool // isolated loopback tests only
	TLSConfig    *tls.Config
	MachineID    string
	AuthToken    string
	Token        func(context.Context) (string, error)
	OnPeerUpdate func([]*pb.PeerUpdate, uint64)
	OnIPAssigned func(string)
	OnError      func(error) // terminal failure; authority has already been withdrawn
}

type Client struct {
	cfg          ClientConfig
	mu           sync.RWMutex
	conn         *grpc.ClientConn
	cancel       context.CancelFunc
	done         chan struct{}
	assignedIP   string
	postureCheck *pb.MachinePostureCheck
	peers        []*pb.PeerUpdate
	revision     uint64
}

func NewClient(cfg ClientConfig) *Client { return &Client{cfg: cfg} }

func (c *Client) token(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ConnectTimeout)
	defer cancel()
	if c.cfg.Token != nil {
		return c.cfg.Token(ctx)
	}
	if c.cfg.AuthToken == "" && !c.cfg.Insecure {
		return "", errors.New("control sync requires authenticated credentials")
	}
	return c.cfg.AuthToken, nil
}
func (c *Client) dial(ctx context.Context) (*grpc.ClientConn, error) {
	addr := strings.TrimPrefix(c.cfg.ServerAddr, "https://")
	if strings.Contains(addr, "://") {
		return nil, errors.New("control sync requires a TLS host:port address")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid sync address: %w", err)
	}
	var creds credentials.TransportCredentials
	if c.cfg.Insecure {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, errors.New("insecure control sync is restricted to literal loopback tests")
		}
		creds = insecure.NewCredentials()
	} else {
		cfg := c.cfg.TLSConfig
		if cfg == nil {
			cfg = &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
		} else {
			cfg = cfg.Clone()
		}
		if cfg.InsecureSkipVerify {
			return nil, errors.New("control sync certificate verification cannot be disabled")
		}
		creds = credentials.NewTLS(cfg)
	}
	dialCtx, cancel := context.WithTimeout(ctx, ConnectTimeout)
	defer cancel()
	return grpc.DialContext(dialCtx, addr, grpc.WithTransportCredentials(creds), grpc.WithBlock())
}

// Start waits for the first authenticated snapshot; subsequent failures withdraw
// the snapshot immediately and retry transient failures until Close or parent
// cancellation. Terminal authority/configuration failures are reported through OnError.
func (c *Client) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	if c.done != nil {
		c.mu.Unlock()
		return errors.New("control sync already started")
	}
	runCtx, cancel := context.WithCancel(ctx)
	runCtx = withSupportReference(runCtx)
	done := make(chan struct{})
	c.cancel, c.done = cancel, done
	c.mu.Unlock()
	conn, stream, streamCancel, err := c.open(runCtx)
	if err != nil {
		err = preserveContextCause(err, runCtx)
		cancel()
		c.mu.Lock()
		c.cancel = nil
		c.done = nil
		c.mu.Unlock()
		close(done)
		if failure := terminalSyncError(err); failure != nil {
			return failure
		}
		return err
	}
	go func() {
		defer close(done)
		retry := newRetryBackoff()
		reportedFailure := false
		for {
			err = c.receive(runCtx, stream, streamCancel)
			streamCancel()
			err = errors.Join(err, conn.Close())
			c.clear()
			err = preserveContextCause(err, runCtx)
			if runCtx.Err() != nil {
				c.finishBackgroundFailure(runCtx, err, reportedFailure)
				return
			}
			if failure := terminalSyncError(err); failure != nil {
				c.finishTerminalFailure(runCtx, failure, reportedFailure)
				return
			}
			if !reportedFailure {
				c.observeSyncFailure(runCtx, err)
				reportedFailure = true
			}
			for {
				if err = waitRetry(runCtx, retry.NextBackOff()); err != nil {
					err = preserveContextCause(err, runCtx)
					c.finishBackgroundFailure(runCtx, err, reportedFailure)
					return
				}
				conn, stream, streamCancel, err = c.open(runCtx)
				if err == nil {
					if reportedFailure {
						if reporter := errorreport.Current(); reporter != nil {
							reporter.Lifecycle(runCtx, "config", "config_sync", "recovered", "success")
						}
					}
					reportedFailure = false
					retry.Reset()
					break
				}
				err = preserveContextCause(err, runCtx)
				if runCtx.Err() != nil {
					c.finishBackgroundFailure(runCtx, err, reportedFailure)
					return
				}
				if failure := terminalSyncError(err); failure != nil {
					c.finishTerminalFailure(runCtx, failure, reportedFailure)
					return
				}
				if !reportedFailure {
					c.observeSyncFailure(runCtx, err)
					reportedFailure = true
				}
			}
		}
	}()
	return nil
}
func (c *Client) open(ctx context.Context) (*grpc.ClientConn, pb.SyncService_SyncClient, context.CancelFunc, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	token, err := c.token(ctx)
	if err != nil {
		return nil, nil, nil, errors.Join(err, conn.Close())
	}
	rpcCtx := withSupportReference(ctx)
	outgoing := metadata.AppendToOutgoingContext(rpcCtx, "authorization", "Bearer "+token)
	if reference := supportref.FromContext(rpcCtx); reference != "" {
		outgoing = metadata.AppendToOutgoingContext(outgoing, supportref.Header, reference)
	}
	streamCtx, cancel := context.WithCancel(outgoing)
	watchdog := time.AfterFunc(SnapshotTimeout, cancel)
	stream, err := pb.NewSyncServiceClient(conn).Sync(streamCtx)
	if err == nil {
		err = stream.Send(&pb.SyncRequest{MachineId: c.cfg.MachineID})
	}
	var response *pb.SyncResponse
	if err == nil {
		response, err = stream.Recv()
	}
	watchdog.Stop()
	if err != nil {
		cancel()
		return nil, nil, nil, errors.Join(err, conn.Close())
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	c.processResponse(response)
	return conn, stream, cancel, nil
}
func (c *Client) receive(ctx context.Context, stream pb.SyncService_SyncClient, cancel context.CancelFunc) error {
	for ctx.Err() == nil {
		watchdog := time.AfterFunc(SnapshotTimeout, cancel)
		resp, err := stream.Recv()
		watchdog.Stop()
		if err != nil {
			return err
		}
		c.processResponse(resp)
	}
	return ctx.Err()
}
func (c *Client) clear() {
	c.mu.Lock()
	c.conn = nil
	c.mu.Unlock()
	c.processResponse(&pb.SyncResponse{})
}
func (c *Client) processResponse(resp *pb.SyncResponse) {
	c.mu.Lock()
	changed := c.assignedIP != resp.GetAssignedIp()
	c.assignedIP = resp.GetAssignedIp()
	c.revision = resp.GetRevision()
	c.postureCheck = nil
	if resp.PostureCheck != nil {
		c.postureCheck = proto.Clone(resp.PostureCheck).(*pb.MachinePostureCheck)
	}
	c.peers = make([]*pb.PeerUpdate, 0, len(resp.Peers))
	for _, peer := range resp.Peers {
		if peer != nil {
			c.peers = append(c.peers, proto.Clone(peer).(*pb.PeerUpdate))
		}
	}
	peers := clonePeers(c.peers)
	ip, rev := c.assignedIP, c.revision
	c.mu.Unlock()
	if changed && c.cfg.OnIPAssigned != nil {
		c.cfg.OnIPAssigned(ip)
	}
	if c.cfg.OnPeerUpdate != nil {
		c.cfg.OnPeerUpdate(peers, rev)
	}
}
func clonePeers(in []*pb.PeerUpdate) []*pb.PeerUpdate {
	out := make([]*pb.PeerUpdate, 0, len(in))
	for _, p := range in {
		out = append(out, proto.Clone(p).(*pb.PeerUpdate))
	}
	return out
}
func (c *Client) Peers() []*pb.PeerUpdate {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return clonePeers(c.peers)
}
func (c *Client) Revision() uint64   { c.mu.RLock(); defer c.mu.RUnlock(); return c.revision }
func (c *Client) AssignedIP() string { c.mu.RLock(); defer c.mu.RUnlock(); return c.assignedIP }
func (c *Client) PostureCheck() *pb.MachinePostureCheck {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.postureCheck == nil {
		return nil
	}
	return proto.Clone(c.postureCheck).(*pb.MachinePostureCheck)
}
func (c *Client) Close() error {
	c.mu.RLock()
	cancel, done := c.cancel, c.done
	c.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	c.mu.Lock()
	if c.done == done {
		c.cancel = nil
		c.done = nil
	}
	c.mu.Unlock()
	return nil
}

// Use the same established backoff primitive as the networking dependencies.
// The upper bound includes jitter so an extended outage never creates unbounded waits.
func newRetryBackoff() *backoff.ExponentialBackOff {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = 800 * time.Millisecond
	b.RandomizationFactor = 0.5
	b.Multiplier = 1.7
	b.MaxInterval = 6 * time.Second
	b.Reset()
	return b
}
func waitRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func terminalSyncError(err error) error {
	code, ok := terminalSyncCode(err)
	if !ok {
		return nil
	}
	message := ""
	switch code {
	case codes.Unauthenticated:
		message = "control sync authentication rejected; local machine access was withdrawn; sign in again and restart the daemon"
	case codes.PermissionDenied:
		message = "control sync authorization denied; local machine access was withdrawn; restore access or select an authorized machine, then restart the daemon"
	case codes.InvalidArgument, codes.FailedPrecondition, codes.Unimplemented:
		message = "control sync configuration or protocol rejected; local machine access was withdrawn; correct the configuration or update Paperboat, then restart the daemon"
	}
	return &syncStatusFailure{grpcStatus: status.New(code, message), cause: err}
}

type syncStatusFailure struct {
	grpcStatus *status.Status
	cause      error
}

func (e *syncStatusFailure) Error() string {
	if e == nil || e.grpcStatus == nil {
		return "control sync failed"
	}
	return e.grpcStatus.Message()
}

func (e *syncStatusFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *syncStatusFailure) GRPCStatus() *status.Status {
	if e == nil || e.grpcStatus == nil {
		return status.New(codes.Internal, "control sync failed")
	}
	return e.grpcStatus
}

func terminalSyncCode(err error) (codes.Code, bool) {
	var code codes.Code
	matched := syncErrorLeavesMatch(err, func(leaf error) bool {
		leafCode := codes.OK
		if leaf == api.ErrUnauthenticated {
			leafCode = codes.Unauthenticated
		} else if grpcErr, ok := leaf.(interface{ GRPCStatus() *status.Status }); ok {
			if grpcStatus := grpcErr.GRPCStatus(); grpcStatus != nil {
				leafCode = grpcStatus.Code()
			}
		}
		switch leafCode {
		case codes.Unauthenticated, codes.PermissionDenied, codes.InvalidArgument, codes.FailedPrecondition, codes.Unimplemented:
		default:
			return false
		}
		if code != codes.OK && leafCode != code {
			return false
		}
		code = leafCode
		return true
	})
	return code, matched
}

func syncErrorLeavesMatch(err error, match func(error) bool) (matched bool) {
	defer func() {
		if recover() != nil {
			matched = false
		}
	}()
	if err == nil || match == nil {
		return false
	}
	pending := []error{err}
	leaves := 0
	for visited := 0; len(pending) > 0; visited++ {
		if visited >= 16 {
			return false
		}
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			return false
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children)+len(pending) > 15-visited {
				return false
			}
			pending = append(pending, children...)
		case interface{ Unwrap() error }:
			child := wrapped.Unwrap()
			if child == nil || len(pending)+1 > 15-visited {
				return false
			}
			pending = append(pending, child)
		default:
			if !match(current) {
				return false
			}
			leaves++
		}
	}
	return leaves > 0
}

func withSupportReference(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if supportref.FromContext(ctx) == "" {
		ctx = supportref.WithContext(ctx, supportref.New())
	}
	return ctx
}

func preserveContextCause(err error, ctx context.Context) error {
	if ctx == nil || ctx.Err() == nil {
		return err
	}
	return errors.Join(err, ctx.Err(), context.Cause(ctx))
}

func (c *Client) observeSyncFailure(ctx context.Context, err error) {
	if err == nil {
		return
	}
	if reporter := errorreport.Current(); reporter != nil {
		reporter.ObserveFailure(ctx, "paperboat-daemon", "config_sync", "control_request", "config_sync_failed", err)
	}
}

func (c *Client) finishTerminalFailure(ctx context.Context, failure error, alreadyReported bool) {
	if c.cfg.OnError != nil {
		c.cfg.OnError(failure)
		return
	}
	if !alreadyReported {
		c.observeSyncFailure(ctx, failure)
	}
}

func (c *Client) finishBackgroundFailure(ctx context.Context, err error, alreadyReported bool) {
	if failure := terminalSyncError(err); failure != nil {
		c.finishTerminalFailure(ctx, failure, alreadyReported)
		return
	}
	if !alreadyReported && !syncErrorLeavesMatch(err, func(leaf error) bool {
		if leaf == context.Canceled || leaf == context.DeadlineExceeded {
			return true
		}
		grpcErr, ok := leaf.(interface{ GRPCStatus() *status.Status })
		if !ok || grpcErr.GRPCStatus() == nil {
			return false
		}
		code := grpcErr.GRPCStatus().Code()
		return code == codes.Canceled || code == codes.DeadlineExceeded
	}) {
		c.observeSyncFailure(ctx, err)
	}
}
