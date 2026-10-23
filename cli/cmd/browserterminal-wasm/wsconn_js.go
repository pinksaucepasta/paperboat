//go:build js && wasm

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall/js"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/browserbroadcast"
)

func newBrowserWSConn(url string, protocols []string) (*browserWSConn, error) {
	if len(protocols) != 2 || protocols[0] != websocketSubprotocol {
		return nil, errors.New("invalid encrypted browser subprotocol")
	}
	constructor := js.Global().Get("WebSocket")
	if constructor.Type() != js.TypeFunction {
		return nil, errors.New("browser WebSocket is unavailable")
	}
	protocolArray := js.Global().Get("Array").New(len(protocols))
	for index, protocolName := range protocols {
		protocolArray.SetIndex(index, protocolName)
	}
	socket := constructor.New(url, protocolArray)
	socket.Set("binaryType", "arraybuffer")
	connection := &browserWSConn{socket: socket, messages: make(chan websocketMessage, websocketQueueDepth), broadcasts: newTerminalReceiveQueue(), opened: make(chan struct{}), done: make(chan struct{}), failure: make(chan error, 1)}
	connection.handlers = append(connection.handlers,
		js.FuncOf(func(_ js.Value, _ []js.Value) any {
			connection.openOnce.Do(func() { close(connection.opened) })
			return nil
		}),
		js.FuncOf(func(_ js.Value, args []js.Value) any {
			if len(args) == 0 {
				connection.fail(errors.New("invalid WebSocket message"))
				return nil
			}
			eventData := args[0].Get("data")
			if eventData.Type() != js.TypeObject {
				connection.fail(errors.New("terminal transport must use binary WebSocket messages"))
				return nil
			}
			bytesValue := js.Global().Get("Uint8Array").New(eventData)
			length := bytesValue.Get("byteLength").Int()
			if length < 1 || length > websocketMessageMax {
				connection.fail(errors.New("WebSocket message size is invalid"))
				return nil
			}
			// Check and reserve encrypted output before allocating/copying it.
			if bytesValue.Index(0).Int() == 1 {
				if length <= 1 || length-1 > browserbroadcast.MaxRecordBytes {
					connection.fail(errors.New("terminal output record size is invalid"))
					return nil
				}
				payload := bytesValue.Call("subarray", 1)
				if err := connection.broadcasts.copyAndEnqueue(length-1, func(data []byte) bool {
					return js.CopyBytesToGo(data, payload) == len(data)
				}); err != nil {
					connection.fail(err)
				}
				return nil
			}
			data := make([]byte, length)
			if js.CopyBytesToGo(data, bytesValue) != length {
				connection.fail(errors.New("WebSocket message could not be copied"))
				return nil
			}
			switch data[0] {
			case 0:
				if len(data) == 1 {
					connection.fail(errors.New("empty TLS envelope"))
					return nil
				}
				select {
				case connection.messages <- websocketMessage{data: data[1:], binary: true}:
				default:
					connection.fail(errors.New("WebSocket receive queue is full"))
				}
			default:
				connection.fail(errors.New("unknown terminal transport envelope"))
			}
			return nil
		}),
		js.FuncOf(func(_ js.Value, _ []js.Value) any {
			connection.fail(errors.New("WebSocket transport failed"))
			return nil
		}),
		js.FuncOf(func(_ js.Value, args []js.Value) any {
			reason := ""
			if len(args) > 0 && args[0].Type() == js.TypeObject {
				reason = args[0].Get("reason").String()
			}
			connection.finish(closeFailure(reason))
			return nil
		}),
	)
	socket.Set("onopen", connection.handlers[0])
	socket.Set("onmessage", connection.handlers[1])
	socket.Set("onerror", connection.handlers[2])
	socket.Set("onclose", connection.handlers[3])
	return connection, nil
}

func (connection *browserWSConn) waitOpen(ctx context.Context) error {
	select {
	case <-connection.opened:
		if connection.socket.Get("protocol").String() != websocketSubprotocol {
			return errors.New("requested encrypted browser subprotocol was not selected")
		}
		return nil
	case <-connection.done:
		return connection.terminalError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (connection *browserWSConn) readInitialMessage(ctx context.Context) (websocketMessage, error) {
	select {
	case message := <-connection.messages:
		return message, nil
	case <-connection.done:
		return websocketMessage{}, connection.terminalError()
	case <-ctx.Done():
		return websocketMessage{}, ctx.Err()
	}
}

func (connection *browserWSConn) Read(target []byte) (int, error) {
	if len(target) == 0 {
		return 0, nil
	}
	for {
		connection.mu.Lock()
		if connection.readOffset < len(connection.readBuffer) {
			count := copy(target, connection.readBuffer[connection.readOffset:])
			connection.readOffset += count
			if connection.readOffset == len(connection.readBuffer) {
				connection.readBuffer = nil
				connection.readOffset = 0
			}
			connection.mu.Unlock()
			return count, nil
		}
		deadline := connection.readDeadline
		connection.mu.Unlock()

		// Drain already received data before returning a close error.
		select {
		case message := <-connection.messages:
			if !message.binary {
				return 0, errors.New("terminal transport must use binary WebSocket messages")
			}
			connection.mu.Lock()
			connection.readBuffer = message.data
			connection.readOffset = 0
			connection.mu.Unlock()
			continue
		default:
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return 0, timeoutError{}
		}
		var timer <-chan time.Time
		var stop *time.Timer
		if !deadline.IsZero() {
			stop = time.NewTimer(time.Until(deadline))
			timer = stop.C
		}
		select {
		case message := <-connection.messages:
			if stop != nil {
				stop.Stop()
			}
			if !message.binary {
				return 0, errors.New("terminal transport must use binary WebSocket messages")
			}
			connection.mu.Lock()
			connection.readBuffer = message.data
			connection.readOffset = 0
			connection.mu.Unlock()
		case <-connection.done:
			if stop != nil {
				stop.Stop()
			}
			return 0, connection.terminalError()
		case err := <-connection.failure:
			if stop != nil {
				stop.Stop()
			}
			return 0, err
		case <-timer:
			return 0, timeoutError{}
		}
	}
}

func (connection *browserWSConn) Write(data []byte) (written int, err error) {
	defer func() {
		if recover() != nil {
			// WebSocket.send may throw if the peer closes between readyState and send.
			written = 0
			err = errors.New("WebSocket send failed")
		}
	}()
	if len(data) == 0 {
		return 0, nil
	}
	if len(data) > websocketMessageMax {
		return 0, errors.New("TLS record exceeds the WebSocket bound")
	}
	connection.writeMu.Lock()
	defer connection.writeMu.Unlock()
	deadline := connection.currentWriteDeadline()
	if deadline.IsZero() {
		deadline = time.Now().Add(websocketWriteWait)
	}
	for connection.socket.Get("readyState").Int() == 1 && connection.socket.Get("bufferedAmount").Int() > websocketWriteLimit {
		// Leave can shorten an active write's wait before graceful detach.
		if updated := connection.currentWriteDeadline(); !updated.IsZero() && updated.Before(deadline) {
			deadline = updated
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return 0, timeoutError{}
		}
		delay := 5 * time.Millisecond
		if !deadline.IsZero() && time.Until(deadline) < delay {
			delay = time.Until(deadline)
		}
		time.Sleep(delay)
	}
	if connection.socket.Get("readyState").Int() != 1 {
		return 0, connection.terminalError()
	}
	bytesValue := js.Global().Get("Uint8Array").New(len(data) + 1)
	wrapped := make([]byte, len(data)+1)
	copy(wrapped[1:], data)
	if js.CopyBytesToJS(bytesValue, wrapped) != len(wrapped) {
		return 0, errors.New("TLS record could not be copied to WebSocket")
	}
	connection.socket.Call("send", bytesValue.Get("buffer"))
	return len(data), nil
}

func (connection *browserWSConn) Close() error {
	connection.closeOnce.Do(func() {
		if connection.socket.Get("readyState").Int() < 2 {
			connection.socket.Call("close", 1000, "closed")
		}
		connection.detachHandlers()
		connection.finish(net.ErrClosed)
	})
	return nil
}

func (connection *browserWSConn) LocalAddr() net.Addr { return browserAddr("browser-local") }

func (connection *browserWSConn) RemoteAddr() net.Addr { return browserAddr("browser-websocket") }

func (connection *browserWSConn) SetDeadline(deadline time.Time) error {
	connection.mu.Lock()
	connection.readDeadline, connection.writeDeadline = deadline, deadline
	connection.mu.Unlock()
	return nil
}

func (connection *browserWSConn) SetReadDeadline(deadline time.Time) error {
	connection.mu.Lock()
	connection.readDeadline = deadline
	connection.mu.Unlock()
	return nil
}

func (connection *browserWSConn) SetWriteDeadline(deadline time.Time) error {
	connection.mu.Lock()
	connection.writeDeadline = deadline
	connection.mu.Unlock()
	return nil
}

func (connection *browserWSConn) fail(err error) {
	select {
	case connection.failure <- err:
	default:
	}
	connection.finish(err)
	if connection.socket.Get("readyState").Int() < 2 {
		connection.socket.Call("close", 1002, "invalid terminal transport")
	}
}

func (connection *browserWSConn) finish(err error) {
	connection.doneOnce.Do(func() {
		connection.broadcasts.close()
		connection.mu.Lock()
		connection.closeErr = err
		connection.mu.Unlock()
		close(connection.done)
	})
}

func (connection *browserWSConn) terminalError() error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if connection.closeErr == nil {
		return io.EOF
	}
	return connection.closeErr
}

func (connection *browserWSConn) currentWriteDeadline() time.Time {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return connection.writeDeadline
}

func (connection *browserWSConn) detachHandlers() {
	for _, name := range []string{"onopen", "onmessage", "onerror", "onclose"} {
		connection.socket.Set(name, js.Null())
	}
	for _, handler := range connection.handlers {
		handler.Release()
	}
	connection.handlers = nil
}

type browserAddr string

func (addr browserAddr) Network() string { return "websocket" }

func (addr browserAddr) String() string { return string(addr) }

type timeoutError struct{}

func (timeoutError) Error() string { return "browser websocket deadline exceeded" }

func (timeoutError) Timeout() bool { return true }

func (timeoutError) Temporary() bool { return true }
