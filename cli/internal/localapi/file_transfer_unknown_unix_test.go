//go:build darwin || linux

package localapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type openErrorBroker struct{ err error }

func (b openErrorBroker) PrepareFileTransfer(context.Context, Peer, FileTransferRequest) (FileTransferResult, error) {
	return FileTransferResult{}, errors.New("not used")
}
func (b openErrorBroker) OpenFileTransferStream(context.Context, Peer, string) (net.Conn, error) {
	return nil, b.err
}
func (openErrorBroker) ReleaseFileTransfer(Peer, string) error { return nil }

// A restarted daemon holds no lease for the handle: 404 with its own code, so the sender prepares a new lease. Any other
// open failure (a draining daemon) stays a retryable 503.
func TestFileTransferStreamAnswersAnUnknownHandleDistinctlyFromAnUnavailableStream(t *testing.T) {
	for name, test := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"unknown handle": {TransferLeaseUnknownError{}, http.StatusNotFound, FileTransferUnknownHandleCode},
		"unavailable":    {errors.New("opener failed"), http.StatusServiceUnavailable, "file_transfer_stream_unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			socket := filepath.Join(localAPITestDir(t), "unknown.sock")
			server, err := NewServer(ServerConfig{SocketPath: socket, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(),
				Source:        snapshotSourceFunc(func(context.Context) (Snapshot, error) { return validSnapshot(), nil }),
				FileTransfers: openErrorBroker{test.err}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go func() { _ = server.Run(ctx) }()
			waitForSocket(t, socket)
			client, err := NewClient(socket, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.OpenFileTransferStream(ctx, "handle_test")
			var remote *RemoteError
			if !errors.As(err, &remote) || remote.StatusCode != test.status || remote.Code != test.code {
				t.Fatalf("err = %v, want %d %s", err, test.status, test.code)
			}
		})
	}
}
