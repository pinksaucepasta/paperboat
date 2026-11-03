package tunnel

import (
	"errors"
	"net"
	"time"
)

// closeForCarrierMove does not detach the logical attachment: the replacement
// takes it over after the old reader has joined. Thus no old detach can arrive
// after a new attach and remove it. The authenticated offer/ACK stream stays up.
func (c *helperTerminalConn) closeForCarrierMove() error {
	message, ok := c.message.(*nativeMessageConnection)
	if !ok {
		return ErrPeerTerminalInvalid
	}
	if c.closing.Swap(true) {
		return net.ErrClosed
	}
	message.closeOnce.Do(func() {
		message.cancel()
		for _, stream := range []nativeStream{message.control, message.input, message.output} {
			if stream == nil {
				continue
			}
			if conn, ok := stream.(net.Conn); ok {
				_ = conn.SetDeadline(time.Now())
			}
			_ = stream.Close()
		}
	})
	c.finish(1, errors.Join(ErrTransportLost, net.ErrClosed))
	return nil
}
