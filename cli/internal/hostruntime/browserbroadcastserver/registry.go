package browserbroadcastserver

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/browserbroadcast"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type Epoch = browserbroadcast.Epoch

var ErrUnavailable = errors.New("browser terminal broadcast unavailable")

// OpenPublisher opens one authenticated host-to-edge stream on the machine's
// existing runtime carrier. The stream carries only opaque COSE output records.
type OpenPublisher func(context.Context, string) (io.WriteCloser, error)

type Registry struct {
	manager    session.Service
	signingKey func(context.Context) (ed25519.PrivateKey, error)
	mu         sync.Mutex
	open       OpenPublisher
	sessions   map[string]*publication
}

type publication struct {
	sessionID     string
	generation    uint64
	attachmentID  string
	epoch         Epoch
	index         uint64
	subscribers   map[string]chan Epoch
	stream        io.WriteCloser
	cancel        context.CancelFunc
	diagnosticCtx context.Context
	mu            sync.Mutex
	stopOnce      sync.Once
}

func NewRegistry(manager session.Service, signingKey func(context.Context) (ed25519.PrivateKey, error)) (*Registry, error) {
	if manager == nil || signingKey == nil {
		return nil, ErrUnavailable
	}
	return &Registry{manager: manager, signingKey: signingKey, sessions: make(map[string]*publication)}, nil
}

// SetPublisher replaces the output stream opener when the runtime carrier
// changes. Existing publications close, which makes browsers reconnect and
// obtain a fresh screen checkpoint and epoch from the new carrier.
func (r *Registry) SetPublisher(open OpenPublisher) {
	r.mu.Lock()
	r.open = open
	old := r.sessions
	r.sessions = make(map[string]*publication)
	r.mu.Unlock()
	for _, p := range old {
		r.stop(p)
	}
}

// Join returns an initial key update and later rotations for one authenticated
// browser attachment. The caller must close the stream with Leave.
func (r *Registry) Join(ctx context.Context, sessionID string, generation uint64, attachmentID string) (<-chan Epoch, error) {
	if r == nil || ctx == nil || sessionID == "" || generation == 0 || attachmentID == "" {
		return nil, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.open == nil {
		return nil, ErrUnavailable
	}
	if p := r.sessions[sessionID]; p != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.generation != generation || len(p.subscribers) >= 100 || p.subscribers[attachmentID] != nil {
			return nil, ErrUnavailable
		}
		updates := make(chan Epoch, 1)
		updates <- p.epoch
		p.subscribers[attachmentID] = updates
		return updates, nil
	}
	if _, err := r.manager.SnapshotAtGeneration(sessionID, generation); err != nil {
		return nil, err
	}
	var workerKey ed25519.PrivateKey
	defer func() { clearPrivateKey(workerKey) }()
	key, err := r.signingKey(ctx)
	if err != nil {
		return nil, classifiedFailure(err, streamOpenStage, sessionCode)
	}
	if len(key) != ed25519.PrivateKeySize {
		return nil, ErrUnavailable
	}
	workerKey = append(ed25519.PrivateKey(nil), key...)
	epoch, err := browserbroadcast.NewEpoch()
	if err != nil {
		return nil, classifiedFailure(err, streamOpenStage, sessionCode)
	}
	publisher, err := r.open(ctx, sessionID)
	if err != nil {
		return nil, classifiedFailure(err, streamOpenStage, transportCode)
	}
	producerAttachment := "broadcast_" + sessionID
	if _, err = r.manager.AttachLive(sessionID, producerAttachment); err != nil {
		return nil, classifiedFailure(errors.Join(err, publisher.Close()), streamOpenStage, sessionCode)
	}
	diagnosticCtx := context.WithoutCancel(ctx)
	if !supportref.Valid(supportref.FromContext(diagnosticCtx)) {
		diagnosticCtx = supportref.WithContext(diagnosticCtx, supportref.New())
	}
	runCtx, cancel := context.WithCancel(diagnosticCtx)
	updates := make(chan Epoch, 1)
	updates <- epoch
	p := &publication{sessionID: sessionID, generation: generation, attachmentID: producerAttachment, epoch: epoch, subscribers: map[string]chan Epoch{attachmentID: updates}, stream: publisher, cancel: cancel, diagnosticCtx: diagnosticCtx}
	r.sessions[sessionID] = p
	go r.publish(runCtx, p, workerKey)
	workerKey = nil
	return updates, nil
}

func (r *Registry) Leave(sessionID, attachmentID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	p := r.sessions[sessionID]
	if p == nil {
		r.mu.Unlock()
		return
	}
	p.mu.Lock()
	updates := p.subscribers[attachmentID]
	if updates == nil {
		p.mu.Unlock()
		r.mu.Unlock()
		return
	}
	delete(p.subscribers, attachmentID)
	close(updates)
	if len(p.subscribers) == 0 {
		delete(r.sessions, sessionID)
		p.mu.Unlock()
		r.mu.Unlock()
		r.stop(p)
		return
	}
	// A former viewer must not decrypt future output even if it later obtains
	// ciphertext from an untrusted edge. Push the new key only over each
	// remaining viewer's authenticated inner TLS stream.
	next, err := browserbroadcast.NewEpoch()
	if err != nil {
		delete(r.sessions, sessionID)
		p.mu.Unlock()
		r.mu.Unlock()
		reportPublisherFailure(p.diagnosticCtx, deliveryStage, err)
		r.stop(p)
		return
	}
	p.epoch = next
	p.index = 0
	for id, subscriber := range p.subscribers {
		select {
		case subscriber <- next:
		default:
			close(subscriber)
			delete(p.subscribers, id)
		}
	}
	if len(p.subscribers) == 0 {
		delete(r.sessions, sessionID)
		p.mu.Unlock()
		r.mu.Unlock()
		r.stop(p)
		return
	}
	p.mu.Unlock()
	r.mu.Unlock()
}

func (r *Registry) stop(p *publication) {
	p.stopOnce.Do(func() {
		p.cancel()
		closeErr := p.stream.Close()
		p.mu.Lock()
		for id, subscriber := range p.subscribers {
			close(subscriber)
			delete(p.subscribers, id)
		}
		p.mu.Unlock()
		detachErr := r.manager.Detach(p.sessionID, p.attachmentID)
		reportPublisherCleanupFailure(p.diagnosticCtx, errors.Join(closeErr, detachErr))
	})
}

func (r *Registry) publish(ctx context.Context, p *publication, key ed25519.PrivateKey) {
	defer func() {
		clearPrivateKey(key)
		r.mu.Lock()
		if r.sessions[p.sessionID] == p {
			delete(r.sessions, p.sessionID)
		}
		r.mu.Unlock()
		r.stop(p)
	}()
	for {
		event, err := r.manager.WaitNext(ctx, p.sessionID, p.attachmentID)
		if err != nil {
			reportPublisherFailure(p.diagnosticCtx, deliveryStage, err)
			return
		}
		p.mu.Lock()
		if p.index == ^uint64(0) {
			p.mu.Unlock()
			event.Release()
			reportPublisherFailure(p.diagnosticCtx, deliveryStage, errOutputIndexExhausted)
			return
		}
		p.index++
		frame, encodeErr := browserbroadcast.Seal(browserbroadcast.Record{SessionID: p.sessionID, Generation: p.generation, EpochID: p.epoch.ID, Index: p.index, Channel: byte(event.Channel), StartSequence: event.StartSequence, Data: event.Data}, p.epoch, key)
		if encodeErr == nil {
			encodeErr = writeRecord(p.stream, frame)
		}
		p.mu.Unlock()
		event.Release()
		if encodeErr != nil {
			reportPublisherFailure(p.diagnosticCtx, deliveryStage, encodeErr)
			return
		}
	}
}

func clearPrivateKey(key ed25519.PrivateKey) {
	for i := range key {
		key[i] = 0
	}
}

func writeRecord(writer io.Writer, payload []byte) error {
	if len(payload) == 0 || len(payload) > browserbroadcast.MaxRecordBytes {
		return browserbroadcast.ErrInvalidRecord
	}
	if deadline, ok := writer.(interface{ SetWriteDeadline(time.Time) error }); ok {
		if err := deadline.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return err
		}
		defer deadline.SetWriteDeadline(time.Time{})
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := writer.Write(header[:]); err != nil {
		return err
	}
	for len(payload) != 0 {
		n, err := writer.Write(payload)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		payload = payload[n:]
	}
	return nil
}
