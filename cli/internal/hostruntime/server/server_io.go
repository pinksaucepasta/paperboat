package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	pberrors "github.com/pinksaucepasta/paperboat/internal/hostruntime/errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

type readResult struct {
	frame  protocol.Frame
	binary []byte
	err    error
}

func readApplication(reader Connection) (protocol.Frame, []byte, error) {
	if application, ok := reader.(ApplicationConnection); ok {
		return application.ReadApplication()
	}
	frame, err := protocol.ReadFrame(reader)
	return frame, nil, err
}

func readFrames(reader Connection, results chan<- readResult, done <-chan struct{}) {
	for {
		frame, binary, err := readApplication(reader)
		select {
		case results <- readResult{frame: frame, binary: binary, err: err}:
		case <-done:
			return
		}
		if err != nil {
			return
		}
	}
}

// lockedWriter serialises one connection's writes. Control frames (input results, heartbeats,
// responses, events) always go before queued terminal output, so a flood never delays the
// answer to a keystroke, a heartbeat or a probe. Output keeps its own order.
type lockedWriter struct {
	writer     io.Writer
	connection Connection
	requests   chan writerRequest
	output     chan writerRequest
	stop       chan struct{}
	done       chan struct{}
	startOnce  sync.Once
	stopOnce   sync.Once
}

type writerRequest struct {
	write  func() error
	result chan error
}

func newLockedWriter(connection Connection) *lockedWriter {
	w := &lockedWriter{writer: connection, connection: connection}
	w.start()
	return w
}

func (w *lockedWriter) start() {
	w.startOnce.Do(func() {
		w.requests = make(chan writerRequest, 256)
		w.output = make(chan writerRequest, 256)
		w.stop = make(chan struct{})
		w.done = make(chan struct{})
		go w.run()
	})
}

func (w *lockedWriter) run() {
	defer close(w.done)
	for {
		var request writerRequest
		select {
		case request = <-w.requests:
		default:
			select {
			case request = <-w.requests:
			case request = <-w.output:
			case <-w.stop:
				return
			}
		}
		err := request.write()
		request.result <- err
		if err != nil {
			return
		}
	}
}

func (w *lockedWriter) enqueue(queue func(*lockedWriter) chan writerRequest, write func() error) error {
	w.start()
	request := writerRequest{write: write, result: make(chan error, 1)}
	select {
	case queue(w) <- request:
	case <-w.done:
		return io.ErrClosedPipe
	}
	select {
	case err := <-request.result:
		return err
	case <-w.done:
		return io.ErrClosedPipe
	}
}

func (w *lockedWriter) close() {
	w.start()
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.done
}

func controlQueue(w *lockedWriter) chan writerRequest { return w.requests }
func outputQueue(w *lockedWriter) chan writerRequest  { return w.output }

func (w *lockedWriter) write(frame protocol.Frame) error {
	return w.enqueue(controlQueue, func() error {
		if connection, ok := w.connection.(FramedConnection); ok {
			return connection.WriteStructured(frame)
		}
		return protocol.WriteFrame(w.writer, frame)
	})
}

func (w *lockedWriter) writeTerminalOutput(streamID uint32, frame protocol.BinaryFrame) error {
	return w.enqueue(outputQueue, func() error {
		if connection, ok := w.connection.(ApplicationConnection); ok {
			return connection.WriteTerminalOutput(streamID, frame)
		}
		return protocol.WriteBinaryFrame(w.writer, frame)
	})
}

func closeReason(err error) string {
	if err == nil || errors.Is(err, io.EOF) {
		return "closed"
	}
	var protocolError *protocol.Error
	if errors.As(err, &protocolError) {
		return string(protocolError.Code)
	}
	return "unavailable"
}

func connectionCloseCode(err error) int {
	if err == nil || errors.Is(err, io.EOF) {
		return protocol.CloseNormal
	}
	return protocol.CloseCode(err)
}

func decodePayload(payload []byte, target any) error {
	decoder := json.NewDecoder(newBytesReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

type byteReader struct{ data []byte }

func newBytesReader(data []byte) *byteReader { return &byteReader{data: data} }

func (r *byteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func errorFrame(requestID, code, message string, retryable bool) protocol.Frame {
	return errorFrameWithDetails(requestID, code, message, retryable, nil)
}

func errorFrameWithDetails(requestID, code, message string, retryable bool, details json.RawMessage) protocol.Frame {
	values := safeErrorDetails(code, details)
	payload, _ := json.Marshal(pberrors.Error{Code: pberrors.Code(code), Message: message, RequestID: requestID, Retryable: retryable, Details: values})
	return protocol.Frame{Type: "error", RequestID: requestID, Version: protocol.ProtocolVersion, Payload: payload}
}

func safeErrorDetails(code string, details json.RawMessage) map[string]any {
	if len(details) == 0 {
		return nil
	}
	if code == "terminal_create_failed" {
		var value struct {
			ErrorType        string `json:"error_type"`
			Errno            int    `json:"errno"`
			SupportReference string `json:"support_reference"`
		}
		if json.Unmarshal(details, &value) != nil || len(value.ErrorType) > 100 || value.Errno < 0 || value.Errno > 65535 {
			return nil
		}
		values := map[string]any{"error_type": value.ErrorType, "errno": value.Errno}
		if supportref.Valid(value.SupportReference) {
			values["support_reference"] = value.SupportReference
		}
		return values
	}
	if code == "slow_consumer" {
		var value struct {
			QueuedBytes uint64 `json:"queued_bytes"`
		}
		if json.Unmarshal(details, &value) != nil {
			return nil
		}
		return map[string]any{"queued_bytes": value.QueuedBytes}
	}
	if code == "stale_generation" {
		var value struct {
			CurrentGeneration uint64 `json:"current_generation"`
		}
		if json.Unmarshal(details, &value) != nil {
			return nil
		}
		return map[string]any{"current_generation": value.CurrentGeneration}
	}
	if code != "replay_gap" {
		return nil
	}
	var gap struct {
		RequestedSequence uint64 `json:"requested_sequence"`
		EarliestSequence  uint64 `json:"earliest_sequence"`
		LatestSequence    uint64 `json:"latest_sequence"`
	}
	if json.Unmarshal(details, &gap) != nil {
		return nil
	}
	return map[string]any{"requested_sequence": gap.RequestedSequence, "earliest_sequence": gap.EarliestSequence, "latest_sequence": gap.LatestSequence}
}

func protocolCode(err error) string {
	var protocolError *protocol.Error
	if errors.As(err, &protocolError) {
		return string(protocolError.Code)
	}
	return "unavailable"
}

func operationErrorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "operation_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, operation.ErrOperationConflict):
		return "operation_id_conflict"
	case errors.Is(err, operation.ErrOperationUncertain):
		return "operation_uncertain"
	case errors.Is(err, operation.ErrJournalFull):
		return "resource_limit"
	default:
		return "unavailable"
	}
}

func operationErrorMessage(code string) string {
	if code == "operation_canceled" {
		return "operation canceled"
	}
	if code == "deadline_exceeded" {
		return "operation deadline exceeded"
	}
	return "operation could not be completed"
}
