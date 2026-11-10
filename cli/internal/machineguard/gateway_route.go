//go:build linux || darwin || windows

package machineguard

import (
	"errors"
	"io"
	"net"
	"strconv"
	"time"
)

// maxGatewayAttributions bounds the connections being attributed at once; more are closed.
const maxGatewayAttributions = 64

func (s *sharedGateway) serve() {
	for {
		accepted, err := s.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			var transient interface{ Temporary() bool }
			if errors.As(err, &transient) && transient.Temporary() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return
		}
		select {
		case s.slots <- struct{}{}:
			go func() {
				defer func() { <-s.slots }()
				s.handoff(accepted)
			}()
		default:
			countGatewayRefusal(refusalBusy, s.port, nil)
			accepted.Close()
		}
	}
}

// refuseGatewayHTTP answers a plain-HTTP gateway connection from a user without a running Paperboat daemon.
func refuseGatewayHTTP(conn net.Conn) {
	const body = "This OS user has no running Paperboat daemon. Run `pb install` and `pb login` as this user.\n"
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.WriteString(conn, "HTTP/1.1 403 Forbidden\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body)
}
