package execprocess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"io"
	"sync"
)

type RemoteCall func(context.Context, string, json.RawMessage) (json.RawMessage, error)
type RemoteManager struct {
	call RemoteCall
	mu   sync.Mutex
	last error
}
type remoteRequest struct {
	ManagedEnvironment []string
	OperationID        string
	Request            *Request
	From               uint64
	Data               []byte
	Signal             pty.Signal
	Dimensions         pty.Dimensions
	ReaderID           uint64
	Ack                uint64
}
type remoteResponse struct {
	Snapshot  Snapshot
	Replay    bool
	Event     Event
	ReaderID  uint64
	Written   int
	Snapshots []Snapshot
}

func NewRemoteManager(call RemoteCall) (*RemoteManager, error) {
	if call == nil {
		return nil, ErrInvalid
	}
	return &RemoteManager{call: call}, nil
}
func (m *RemoteManager) LastError() error { m.mu.Lock(); defer m.mu.Unlock(); return m.last }
func (m *RemoteManager) invoke(ctx context.Context, op string, request remoteRequest) (remoteResponse, error) {
	var out remoteResponse
	body, err := json.Marshal(request)
	if err == nil {
		body, err = m.call(ctx, op, body)
	}
	if err == nil {
		err = json.Unmarshal(body, &out)
	}
	m.mu.Lock()
	m.last = err
	m.mu.Unlock()
	return out, err
}
func (m *RemoteManager) Start(ctx context.Context, request Request) (ExecutionService, bool, error) {
	out, err := m.invoke(ctx, "start", remoteRequest{Request: &request, ManagedEnvironment: LaunchEnvironment(ctx)})
	if err != nil {
		return nil, false, err
	}
	return &remoteExecution{manager: m, id: out.Snapshot.OperationID, snapshot: out.Snapshot}, out.Replay, nil
}
func (m *RemoteManager) Get(id string) (ExecutionService, error) {
	out, err := m.invoke(context.Background(), "get", remoteRequest{OperationID: id})
	if err != nil {
		return nil, err
	}
	return &remoteExecution{manager: m, id: id, snapshot: out.Snapshot}, nil
}
func (m *RemoteManager) ActiveSnapshots() []Snapshot {
	out, _ := m.invoke(context.Background(), "list", remoteRequest{})
	return out.Snapshots
}

type remoteExecution struct {
	manager  *RemoteManager
	id       string
	mu       sync.Mutex
	snapshot Snapshot
}

func (e *remoteExecution) Snapshot() Snapshot {
	out, err := e.invoke(context.Background(), "get", remoteRequest{})
	e.mu.Lock()
	defer e.mu.Unlock()
	if err == nil {
		e.snapshot = out.Snapshot
	}
	return e.snapshot
}
func (e *remoteExecution) invoke(ctx context.Context, op string, r remoteRequest) (remoteResponse, error) {
	r.OperationID = e.id
	return e.manager.invoke(ctx, op, r)
}
func (e *remoteExecution) Wait(ctx context.Context) (Snapshot, error) {
	out, err := e.invoke(ctx, "wait", remoteRequest{})
	if err == nil {
		e.mu.Lock()
		e.snapshot = out.Snapshot
		e.mu.Unlock()
	}
	return out.Snapshot, err
}
func (e *remoteExecution) Next(ctx context.Context, from uint64) (Event, error) {
	out, err := e.invoke(ctx, "next", remoteRequest{From: from})
	return out.Event, err
}
func (e *remoteExecution) OpenReader(from uint64) (ReaderService, error) {
	out, err := e.invoke(context.Background(), "reader_open", remoteRequest{From: from})
	if err != nil {
		return nil, err
	}
	return &remoteReader{execution: e, id: out.ReaderID}, nil
}
func (e *remoteExecution) Write(data []byte) (int, error) {
	out, err := e.invoke(context.Background(), "write", remoteRequest{Data: data})
	return out.Written, err
}
func (e *remoteExecution) CloseInput() error {
	_, err := e.invoke(context.Background(), "close_input", remoteRequest{})
	return err
}
func (e *remoteExecution) Signal(signal pty.Signal) error {
	_, err := e.invoke(context.Background(), "signal", remoteRequest{Signal: signal})
	return err
}
func (e *remoteExecution) Resize(d pty.Dimensions) error {
	_, err := e.invoke(context.Background(), "resize", remoteRequest{Dimensions: d})
	return err
}
func (e *remoteExecution) Cancel(ctx context.Context) error {
	_, err := e.invoke(ctx, "cancel", remoteRequest{})
	return err
}

type remoteReader struct {
	execution *remoteExecution
	id        uint64
	mu        sync.Mutex
	ack       uint64
	closed    bool
}

func (r *remoteReader) Next(ctx context.Context) (Event, func(), error) {
	r.mu.Lock()
	ack := r.ack
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return Event{}, nil, io.EOF
	}
	out, err := r.execution.invoke(ctx, "reader_next", remoteRequest{ReaderID: r.id, Ack: ack})
	if err != nil {
		return Event{}, nil, err
	}
	var once sync.Once
	return out.Event, func() {
		once.Do(func() {
			r.mu.Lock()
			if out.Event.Sequence+1 > r.ack {
				r.ack = out.Event.Sequence + 1
			}
			r.mu.Unlock()
			_, _ = r.execution.invoke(context.Background(), "reader_ack", remoteRequest{ReaderID: r.id, Ack: out.Event.Sequence + 1})
		})
	}, nil
}
func (r *remoteReader) Close() error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	_, err := r.execution.invoke(context.Background(), "reader_close", remoteRequest{ReaderID: r.id})
	return err
}

type RemoteServer struct {
	Manager        Service
	MaximumReaders int
	mu             sync.Mutex
	next           uint64
	readers        map[uint64]*remoteReaderOwner
}
type remoteReaderOwner struct {
	cancel        context.CancelFunc
	readerContext context.Context
	operationID   string
	reader        ReaderService
	mu            sync.Mutex
	event         *Event
	release       func()
	ack           uint64
	closed        bool
}

func (s *RemoteServer) ResetReaders() error {
	s.mu.Lock()
	readers := s.readers
	s.readers = make(map[uint64]*remoteReaderOwner)
	s.mu.Unlock()
	var result error
	for _, r := range readers {
		r.cancel()
		r.mu.Lock()
		r.closed = true
		if r.release != nil {
			r.release()
		}
		result = errors.Join(result, r.reader.Close())
		r.mu.Unlock()
	}
	return result
}
func (s *RemoteServer) Call(ctx context.Context, op string, body json.RawMessage) (json.RawMessage, error) {
	var request remoteRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return nil, ErrInvalid
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, ErrInvalid
	}
	var out remoteResponse
	if op == "start" {
		if request.Request == nil {
			return nil, ErrInvalid
		}
		e, replay, err := s.Manager.Start(WithLaunchEnvironment(ctx, request.ManagedEnvironment), *request.Request)
		if err != nil {
			return nil, err
		}
		out.Snapshot, out.Replay = e.Snapshot(), replay
		return json.Marshal(out)
	}
	if op == "list" {
		out.Snapshots = s.Manager.ActiveSnapshots()
		return json.Marshal(out)
	}
	if op == "reader_open" {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.MaximumReaders <= 0 || len(s.readers) >= s.MaximumReaders {
			return nil, ErrCapacity
		}
		e, err := s.Manager.Get(request.OperationID)
		if err != nil {
			return nil, err
		}
		reader, err := e.OpenReader(request.From)
		if err != nil {
			return nil, err
		}
		if s.readers == nil {
			s.readers = make(map[uint64]*remoteReaderOwner)
		}
		s.next++
		if s.next == 0 {
			_ = reader.Close()
			return nil, ErrCapacity
		}
		readerContext, cancel := context.WithCancel(context.Background())
		s.readers[s.next] = &remoteReaderOwner{operationID: request.OperationID, reader: reader, cancel: cancel, readerContext: readerContext}
		out.ReaderID = s.next
		return json.Marshal(out)
	}
	if op == "reader_next" || op == "reader_ack" || op == "reader_close" {
		s.mu.Lock()
		r := s.readers[request.ReaderID]
		if op == "reader_close" && r != nil && r.operationID == request.OperationID {
			delete(s.readers, request.ReaderID)
		}
		s.mu.Unlock()
		if r == nil {
			if op == "reader_close" {
				return json.Marshal(out)
			}
			return nil, ErrNotFound
		}
		if r.operationID != request.OperationID {
			return nil, ErrInvalid
		}
		if op == "reader_close" {
			r.cancel()
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.closed {
			return nil, ErrNotFound
		}
		if request.Ack != 0 && request.Ack > r.ack {
			if r.event == nil || request.Ack != r.event.Sequence+1 {
				return nil, ErrInvalid
			}
			r.release()
			r.ack = request.Ack
			r.release = nil
			r.event = nil
		}
		switch op {
		case "reader_close":
			r.closed = true
			if r.release != nil {
				r.release()
			}
			if err := r.reader.Close(); err != nil {
				return nil, err
			}
		case "reader_next":
			if r.event == nil {
				callContext, cancel := context.WithCancel(ctx)
				stop := context.AfterFunc(r.readerContext, cancel)
				event, release, err := r.reader.Next(callContext)
				stop()
				cancel()
				if err != nil {
					return nil, err
				}
				r.event = &event
				r.release = release
			}
			out.Event = *r.event
		}
		return json.Marshal(out)
	}
	e, err := s.Manager.Get(request.OperationID)
	if err != nil {
		return nil, err
	}
	switch op {
	case "get":
		out.Snapshot = e.Snapshot()
	case "wait":
		out.Snapshot, err = e.Wait(ctx)
	case "next":
		out.Event, err = e.Next(ctx, request.From)
	case "write":
		owner, ok := e.(interface {
			WriteContext(context.Context, []byte) (int, error)
		})
		if !ok {
			return nil, ErrInvalid
		}
		out.Written, err = owner.WriteContext(ctx, request.Data)
	case "close_input":
		err = e.CloseInput()
	case "signal":
		err = e.Signal(request.Signal)
	case "resize":
		err = e.Resize(request.Dimensions)
	case "cancel":
		err = e.Cancel(ctx)
	default:
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}
