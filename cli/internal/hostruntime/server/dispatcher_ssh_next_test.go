package server

import (
	"context"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/protocol"
)

// A read that already took bytes out of the pipe keeps them when the request is canceled at the same moment: both
// are ready and a random pick dropped the bytes as a cancel.
func TestSSHOutputStreamAwaitKeepsBytesAlreadyReadWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := range 200 { // a select with both cases ready picks one at random
		stream := newSSHOutputStream("operation_ssh_0001", nil)
		buffer := []byte("opaque-ssh-bytes")
		result := make(chan sshRead, 1)
		result <- sshRead{count: len(buffer)}
		frame, err := stream.await(ctx, buffer, result)
		if err != nil || string(frame.Data) != "opaque-ssh-bytes" || frame.Channel != protocol.Stdout {
			t.Fatalf("draw %d: bytes already read were reported as frame=%#v err=%v", i, frame, err)
		}
	}
	stream := newSSHOutputStream("operation_ssh_0001", nil)
	if _, err := stream.await(ctx, make([]byte, 8), make(chan sshRead, 1)); err != context.Canceled {
		t.Fatalf("a canceled wait with nothing read reported %v, want %v", err, context.Canceled)
	}
}
