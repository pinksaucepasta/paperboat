package tunnel

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

const (
	nativeVersion     byte = 1
	nativeRoleControl byte = 1
	nativeRoleInput   byte = 2
	nativeRoleOutput  byte = 3
	nativeRoleUnified byte = 4
	nativeBindingSize      = 32
	nativeMaxRecord        = 1 << 20
	nativeStructured  byte = 1
	nativeBinary      byte = 2
)

var nativeMagic = [4]byte{'P', 'B', 'T', '1'}

type nativeStream interface {
	io.ReadWriteCloser
	SetWriteDeadline(time.Time) error
}

type nativeStreamGroup interface {
	OpenStream(context.Context) (nativeStream, error)
	Close() error
}

type terminalTransportError struct {
	transport string
	cause     error
}

var errInvalidNativeWelcome = errors.New("helper returned an invalid native protocol welcome")
var ErrPeerStreamOpen = errors.New("peer application stream open failed")

func (e *terminalTransportError) Error() string {
	return fmt.Sprintf("%s terminal transport unavailable: %v", e.transport, e.cause)
}
func (e *terminalTransportError) Unwrap() error { return e.cause }
func FallbackEligible(err error) bool {
	var target *terminalTransportError
	return errors.As(err, &target)
}

func authenticateNativeStreamGroup(ctx context.Context, connection nativeStreamGroup, target *resolver.TerminalTarget, transport string) (*nativeMessageConnection, error) {
	var id [16]byte
	if _, err := io.ReadFull(rand.Reader, id[:]); err != nil {
		_ = connection.Close()
		return nil, err
	}
	control, err := connection.OpenStream(ctx)
	if err != nil {
		_ = connection.Close()
		return nil, errors.Join(ErrPeerStreamOpen, &terminalTransportError{transport: transport, cause: err})
	}
	if err := writeNativePreface(control, nativeRoleControl, id, nil, target.Auth.Token); err != nil {
		_ = connection.Close()
		return nil, classifyNativeHandshakeError(ctx, transport, err)
	}
	message := newNativeMessageConnection(connection, control)
	binding, err := nativeHandshake(ctx, message)
	if err != nil {
		_ = message.Close()
		return nil, classifyNativeHandshakeError(ctx, transport, err)
	}
	var input, output nativeStream
	// The host admits auxiliary streams in this order. Opening concurrently
	// allows relay multiplexing to deliver output before input, causing a
	// stream-scoped authority mismatch even though the transport is valid.
	for _, role := range []byte{nativeRoleInput, nativeRoleOutput} {
		stream, openErr := connection.OpenStream(ctx)
		if openErr == nil {
			openErr = writeNativePreface(stream, role, id, binding, "")
		}
		if openErr != nil {
			if stream != nil {
				_ = stream.Close()
			}
			if input != nil {
				_ = input.Close()
			}
			if output != nil {
				_ = output.Close()
			}
			_ = message.Close()
			return nil, &terminalTransportError{transport: transport, cause: openErr}
		}
		if role == nativeRoleInput {
			input = stream
		} else {
			output = stream
		}
	}
	message.attach(input, output)
	return message, nil
}

func authenticateUnifiedNativeStream(ctx context.Context, connection nativeStreamGroup, target *resolver.TerminalTarget, transport string) (*nativeMessageConnection, error) {
	var id [16]byte
	if _, err := io.ReadFull(rand.Reader, id[:]); err != nil {
		_ = connection.Close()
		return nil, err
	}
	control, err := connection.OpenStream(ctx)
	if err != nil {
		_ = connection.Close()
		return nil, errors.Join(ErrPeerStreamOpen, &terminalTransportError{transport: transport, cause: err})
	}
	if err := writeNativePreface(control, nativeRoleUnified, id, nil, target.Auth.Token); err != nil {
		_ = connection.Close()
		return nil, classifyNativeHandshakeError(ctx, transport, err)
	}
	message := newNativeMessageConnection(connection, control)
	message.unified = true
	if _, err := nativeHandshake(ctx, message); err != nil {
		_ = message.Close()
		return nil, classifyNativeHandshakeError(ctx, transport, err)
	}
	return message, nil
}

func classifyNativeHandshakeError(ctx context.Context, transport string, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var remote *helperRemoteError
	if errors.As(err, &remote) && !remote.Retryable || errors.Is(err, errInvalidNativeWelcome) {
		return err
	}
	return &terminalTransportError{transport: transport, cause: err}
}

func nativeHandshake(ctx context.Context, message helperMessageConnection) ([]byte, error) {
	payload, _ := json.Marshal(map[string]any{"min_version": helperProtocolVersion, "max_version": helperProtocolVersion, "capabilities": helperCapabilities()})
	id := helperID("req_")
	if err := writeHelperFrame(ctx, message, helperFrame{Type: "hello", RequestID: id, Version: helperProtocolVersion, Payload: payload}); err != nil {
		return nil, err
	}
	frame, err := readHelperStructured(ctx, message)
	if err != nil {
		return nil, err
	}
	if frame.Type == "error" {
		return nil, decodeHelperError(frame)
	}
	var welcome struct {
		Version      string   `json:"version"`
		Capabilities []string `json:"capabilities"`
		Binding      []byte   `json:"binding_secret"`
	}
	if frame.Type != "welcome" || frame.RequestID != id || json.Unmarshal(frame.Payload, &welcome) != nil || welcome.Version != helperProtocolVersion || len(welcome.Binding) != nativeBindingSize || !containsString(welcome.Capabilities, "terminal.v1") || !containsString(welcome.Capabilities, "health.v1") {
		return nil, errInvalidNativeWelcome
	}
	return welcome.Binding, nil
}

type nativeMessageConnection struct {
	connection nativeStreamGroup
	control    nativeStream
	input      nativeStream
	output     nativeStream
	ctx        context.Context
	cancel     context.CancelFunc
	reads      chan nativeMessage
	writeMu    sync.Mutex
	closeOnce  sync.Once
	unified    bool
}
type nativeMessage struct {
	kind helperMessageType
	data []byte
	err  error
}

func newNativeMessageConnection(connection nativeStreamGroup, control nativeStream) *nativeMessageConnection {
	ctx, cancel := context.WithCancel(context.Background())
	c := &nativeMessageConnection{connection: connection, control: control, ctx: ctx, cancel: cancel, reads: make(chan nativeMessage, 2)}
	go c.read(control, true)
	return c
}
func (c *nativeMessageConnection) attach(input, output nativeStream) {
	c.input, c.output = input, output
	go c.read(output, false)
}
func (c *nativeMessageConnection) read(stream io.Reader, typed bool) {
	for {
		kind, data, err := readNativeRecord(stream, typed)
		// Auxiliary output closure is independent of the authenticated control
		// stream. Do not turn it into a connection-wide EOF before control can
		// deliver the terminal result.
		if err != nil && !typed {
			return
		}
		messageKind := helperBinaryMessage
		if kind == nativeStructured {
			messageKind = helperStructuredMessage
		}
		select {
		case c.reads <- nativeMessage{messageKind, data, err}:
		case <-c.ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}
func (c *nativeMessageConnection) ReadMessage(ctx context.Context) (helperMessageType, []byte, error) {
	select {
	case result := <-c.reads:
		return result.kind, result.data, result.err
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-c.ctx.Done():
		return 0, nil, io.EOF
	}
}
func (c *nativeMessageConnection) WriteMessage(ctx context.Context, kind helperMessageType, data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	stream := c.control
	typed := true
	recordKind := nativeStructured
	if kind == helperBinaryMessage {
		recordKind = nativeBinary
		if !c.unified && len(data) > 0 && (data[0] == protocol.TerminalInputOpcode || data[0] == protocol.TerminalEOFOpcode) {
			if c.input == nil {
				return errors.New("native input stream unavailable")
			}
			stream = c.input
			typed = false
		}
	} else if kind != helperStructuredMessage {
		return errors.New("invalid helper message type")
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = stream.SetWriteDeadline(deadline)
	}
	cancelDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = stream.SetWriteDeadline(time.Now()); close(cancelDone) })
	err := writeNativeRecord(stream, recordKind, data, typed)
	if !stop() {
		<-cancelDone
	}
	_ = stream.SetWriteDeadline(time.Time{})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if c.ctx.Err() != nil {
		return io.EOF
	}
	return err
}
func (c *nativeMessageConnection) Close() error {
	c.closeOnce.Do(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		for _, stream := range []nativeStream{c.control, c.input, c.output} {
			if stream == nil {
				continue
			}
			if half, ok := stream.(interface{ CloseWrite() error }); ok {
				_ = half.CloseWrite()
			}
			if graceful, ok := stream.(interface{ WaitWriteClosed(context.Context) error }); ok {
				_ = graceful.WaitWriteClosed(closeCtx)
			}
		}
		c.cancel()
		_ = c.connection.Close()
	})
	return nil
}

func writeNativePreface(w io.Writer, role byte, id [16]byte, binding []byte, token string) error {
	buffer := make([]byte, 26+len(binding)+len(token))
	copy(buffer, nativeMagic[:])
	buffer[4], buffer[5] = nativeVersion, role
	copy(buffer[6:22], id[:])
	binary.BigEndian.PutUint16(buffer[22:24], uint16(len(binding)))
	binary.BigEndian.PutUint16(buffer[24:26], uint16(len(token)))
	copy(buffer[26:], binding)
	copy(buffer[26+len(binding):], token)
	if bound, ok := w.(interface{ WriteFirst([]byte) error }); ok {
		return bound.WriteFirst(buffer)
	}
	return writeNativeFull(w, buffer)
}
func readNativeRecord(r io.Reader, typed bool) (byte, []byte, error) {
	size := 4
	if typed {
		size = 5
	}
	header := make([]byte, size)
	if _, err := io.ReadFull(r, header); err != nil {
		return 0, nil, err
	}
	kind := nativeBinary
	offset := 0
	if typed {
		kind = header[0]
		offset = 1
		if kind != nativeStructured && kind != nativeBinary {
			return 0, nil, errors.New("invalid native record kind")
		}
	}
	length := binary.BigEndian.Uint32(header[offset:])
	if length == 0 || length > nativeMaxRecord {
		return 0, nil, errors.New("invalid native record length")
	}
	data := make([]byte, length)
	_, err := io.ReadFull(r, data)
	return kind, data, err
}
func writeNativeRecord(w io.Writer, kind byte, data []byte, typed bool) error {
	if len(data) == 0 || len(data) > nativeMaxRecord || typed && kind != nativeStructured && kind != nativeBinary {
		return errors.New("invalid native record length")
	}
	size := 4
	if typed {
		size = 5
	}
	record := make([]byte, size+len(data))
	if typed {
		record[0] = kind
	}
	binary.BigEndian.PutUint32(record[size-4:size], uint32(len(data)))
	copy(record[size:], data)
	return writeNativeFull(w, record)
}
func writeNativeFull(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
