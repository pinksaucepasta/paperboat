package hostdproto

import (
	"errors"
	"io"
	"net"
	"time"
)

// HandoverEnv names the old generation's handover socket for a new hostd.
const HandoverEnv = "PAPERBOAT_HOSTD_HANDOVER_SOCKET"

// ErrNoHandover means no generation answers on the handover socket.
var ErrNoHandover = errors.New("no running hostd generation to take over from")

const (
	handoverStart  = 'S'
	handoverCommit = 'C'
	handoverAck    = 'A'
	// The new generation starts its worker between start and commit.
	handoverCommitTimeout = 5 * time.Minute
	handoverStepTimeout   = 10 * time.Second
)

// HandoverPath is the fixed handover socket next to the lifecycle socket.
func HandoverPath(socket string) string { return socket + ".handover" }

func expectHandoverByte(connection net.Conn, want byte, timeout time.Duration) error {
	if err := connection.SetDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	got := make([]byte, 1)
	if _, err := io.ReadFull(connection, got); err != nil {
		return err
	}
	if got[0] != want {
		return ErrInvalidFrame
	}
	return nil
}
