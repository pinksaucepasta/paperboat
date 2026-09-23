// Package controlsync consumes authorized full control-plane snapshots. It owns
// connection recovery, never admission or device identity.
package controlsync

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/pinksaucepasta/paperboat/internal/api"
	pb "github.com/pinksaucepasta/paperboat/internal/controlsync/proto"
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
	DeviceID     string
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
	postureCheck *pb.DevicePostureCheck
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
	c.mu.Lock()
	if c.done != nil {
		c.mu.Unlock()
		return errors.New("control sync already started")
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	c.cancel, c.done = cancel, done
	c.mu.Unlock()
	conn, stream, streamCancel, err := c.open(runCtx)
	if err != nil {
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
		for {
			err = c.receive(runCtx, stream, streamCancel)
			streamCancel()
			_ = conn.Close()
			c.clear()
			if runCtx.Err() != nil {
				return
			}
			for {
				if failure := terminalSyncError(err); failure != nil {
					if c.cfg.OnError != nil {
						c.cfg.OnError(failure)
					}
					return
				}
				if err = waitRetry(runCtx, retry.NextBackOff()); err != nil {
					return
				}
				conn, stream, streamCancel, err = c.open(runCtx)
				if err == nil {
					retry.Reset()
					break
				}
				if runCtx.Err() != nil {
					return
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
		conn.Close()
		return nil, nil, nil, err
	}
	streamCtx, cancel := context.WithCancel(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token))
	watchdog := time.AfterFunc(SnapshotTimeout, cancel)
	stream, err := pb.NewSyncServiceClient(conn).Sync(streamCtx)
	if err == nil {
		err = stream.Send(&pb.SyncRequest{DeviceId: c.cfg.DeviceID})
	}
	var response *pb.SyncResponse
	if err == nil {
		response, err = stream.Recv()
	}
	watchdog.Stop()
	if err != nil {
		cancel()
		conn.Close()
		return nil, nil, nil, err
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
		c.postureCheck = proto.Clone(resp.PostureCheck).(*pb.DevicePostureCheck)
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
func (c *Client) PostureCheck() *pb.DevicePostureCheck {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.postureCheck == nil {
		return nil
	}
	return proto.Clone(c.postureCheck).(*pb.DevicePostureCheck)
}
func (c *Client) SetTags(ctx context.Context, deviceID string, tags []string) error {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return errors.New("control sync unavailable; reconnect before changing tags")
	}
	token, err := c.token(ctx)
	if err != nil {
		return err
	}
	rpcCtx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token), ConnectTimeout)
	defer cancel()
	_, err = pb.NewSyncServiceClient(conn).SetTags(rpcCtx, &pb.SetTagsRequest{DeviceId: deviceID, Tags: tags})
	return err
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
	if errors.Is(err, api.ErrUnauthenticated) {
		err = status.Error(codes.Unauthenticated, "credential refresh rejected")
	}
	switch status.Code(err) {
	case codes.Unauthenticated:
		return status.Error(codes.Unauthenticated, "control sync authentication rejected; local device access was withdrawn; sign in again and restart the daemon")
	case codes.PermissionDenied:
		return status.Error(codes.PermissionDenied, "control sync authorization denied; local device access was withdrawn; restore access or select an authorized device, then restart the daemon")
	case codes.InvalidArgument, codes.FailedPrecondition, codes.Unimplemented:
		return status.Error(status.Code(err), "control sync configuration or protocol rejected; local device access was withdrawn; correct the configuration or update Paperboat, then restart the daemon")
	default:
		return nil
	}
}
