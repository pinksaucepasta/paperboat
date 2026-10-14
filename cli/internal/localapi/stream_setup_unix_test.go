//go:build darwin || linux

package localapi

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestFileTransferSetupCancellationAndSupportReference(t *testing.T) {
	for _, kind := range []string{"prepare", "stream"} {
		t.Run(kind, func(t *testing.T) {
			socket := filepath.Join(localAPITestDir(t), "setup.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			client, err := NewClient(socket, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			reference := supportref.New()
			ctx, cancel := context.WithCancel(supportref.WithContext(t.Context(), reference))
			defer cancel()
			result := make(chan error, 1)
			go func() {
				if kind == "prepare" {
					lease, err := client.PrepareFileTransfer(ctx, FileTransferRequest{Schema: FileTransferSchemaV1, MachineID: "machine_test", EnvironmentID: "environment_test", MachineGeneration: 1, OperationID: "operation_test", Credential: "credential_test", AccessSessionID: "access_test", Deadline: time.Now().Add(time.Minute), MaximumBytes: 1024})
					if lease != nil {
						_ = lease.Close()
					}
					result <- err
				} else {
					stream, err := client.OpenFileTransferStream(ctx, "handle_test")
					if stream != nil {
						_ = stream.Close()
					}
					result <- err
				}
			}()
			connection, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if err := connection.SetDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			request, err := http.ReadRequest(bufio.NewReader(connection))
			if err != nil {
				t.Fatal(err)
			}
			defer request.Body.Close()
			if request.Header.Get(supportref.Header) != reference {
				t.Fatal("transfer setup lost its support reference")
			}
			// The daemon accepted the request but has not returned headers. Caller
			// cancellation must interrupt this read rather than strand setup.
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatal("setup cancellation lost its cause")
				}
			case <-time.After(time.Second):
				t.Fatal("setup read survived cancellation")
			}
		})
	}
}
