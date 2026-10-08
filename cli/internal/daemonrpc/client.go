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

// StreamStatus subscribes to the live daemon status stream.
func (c *Client) StreamStatus(ctx context.Context) (pb.DaemonService_StreamStatusClient, error) {
	return c.client.StreamStatus(rpcContext(ctx), &pb.Empty{})
}

// ResolveMachine queries the daemon to resolve a peer machine to its 127.100.x.x IP and ports.
func (c *Client) ResolveMachine(ctx context.Context, query string) (*pb.MachineAddress, error) {
	return c.client.ResolveMachine(rpcContext(ctx), &pb.MachineQuery{Query: query})
}

// SetMachineTags sets or replaces the tags associated with a machine.
func (c *Client) SetMachineTags(ctx context.Context, machineID string, tags []string) (*pb.SetMachineTagsResponse, error) {
	return c.client.SetMachineTags(rpcContext(ctx), &pb.SetMachineTagsRequest{
		MachineId: machineID,
		Tags:      tags,
	})
}

// ApprovePeer approves or revokes peer access.
func (c *Client) ApprovePeer(ctx context.Context, machineID string, approved bool) (*pb.ApprovePeerResponse, error) {
	return c.client.ApprovePeer(rpcContext(ctx), &pb.ApprovePeerRequest{
		MachineId: machineID,
		Approved:  approved,
	})
}
