package daemonrpc

import (
	"context"
	"fmt"

	pb "github.com/pinksaucepasta/paperboat/internal/daemonrpc/proto"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const supportReferenceMetadata = "support-reference"

// Client is a gRPC client communicating with the Paperboat daemon.
type Client struct {
	conn   *grpc.ClientConn
	client pb.DaemonServiceClient
}

func rpcContext(ctx context.Context) context.Context {
	if reference := supportref.FromContext(ctx); reference != "" {
		return metadata.AppendToOutgoingContext(ctx, supportReferenceMetadata, reference)
	}
	return ctx
}

// NewClient dials the local daemon gRPC IPC at the specified address and returns a ready client.
func NewClient(ctx context.Context, addr string) (*Client, error) {
	if addr == "" {
		addr = DefaultSocketAddress()
	}
	conn, err := Dial(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("dial daemon at %s: %w", addr, err)
	}

	return &Client{
		conn:   conn,
		client: pb.NewDaemonServiceClient(conn),
	}, nil
}

// Close closes the underlying gRPC client connection.
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// RawClient returns the generated gRPC client for direct access.
func (c *Client) RawClient() pb.DaemonServiceClient {
	return c.client
}

// GetStatus fetches the current daemon status and peer table snapshot by receiving the initial frame.
func (c *Client) GetStatus(ctx context.Context) (*pb.StatusResponse, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.client.StreamStatus(rpcContext(ctx), &pb.Empty{})
	if err != nil {
		return nil, err
	}
	return stream.Recv()
}

// StreamStatus subscribes to the live daemon status stream.
func (c *Client) StreamStatus(ctx context.Context) (pb.DaemonService_StreamStatusClient, error) {
	return c.client.StreamStatus(rpcContext(ctx), &pb.Empty{})
}

// ResolveDevice queries the daemon to resolve a peer device to its 127.100.x.x IP and ports.
func (c *Client) ResolveDevice(ctx context.Context, query string) (*pb.DeviceAddress, error) {
	return c.client.ResolveDevice(rpcContext(ctx), &pb.DeviceQuery{Query: query})
}

// SetDeviceTags sets or replaces the tags associated with a device.
func (c *Client) SetDeviceTags(ctx context.Context, deviceID string, tags []string) (*pb.SetDeviceTagsResponse, error) {
	return c.client.SetDeviceTags(rpcContext(ctx), &pb.SetDeviceTagsRequest{
		DeviceId: deviceID,
		Tags:     tags,
	})
}

// ApprovePeer approves or revokes peer access.
func (c *Client) ApprovePeer(ctx context.Context, deviceID string, approved bool) (*pb.ApprovePeerResponse, error) {
	return c.client.ApprovePeer(rpcContext(ctx), &pb.ApprovePeerRequest{
		DeviceId: deviceID,
		Approved: approved,
	})
}
