package browserbroadcastserver

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"io"
	"sync"
	"sync/atomic"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/browserbroadcast"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

type Epoch = browserbroadcast.Epoch

var ErrUnavailable = errors.New("browser terminal broadcast unavailable")

// OpenPublisher opens one authenticated host-to-edge stream on the machine's
// existing runtime carrier. The stream carries only opaque COSE output records.
// The context is the stream's whole lifetime: the carrier resets the stream when
// it ends, so never pass a request- or deadline-scoped context.
type OpenPublisher func(context.Context, string) (io.WriteCloser, error)

// CarrierHeader names, on a request the machine's runtime carrier forwards to its own listener, the carrier it
// arrived on. The carrier's forwarder sets it, replacing anything the client sent.
const CarrierHeader = "X-Paperboat-Runtime-Carrier"

// maxSubscribers bounds one session's browser attachments over all carriers.
const maxSubscribers = 100

// A machine holds a carrier on each edge of its pair, and a browser may attach through any of them. An edge only
// forwards the records its own carrier receives, so output is published per carrier: on the one each browser came
// through. Publishing on one chosen carrier left every browser on another edge attached but silent.
type Registry struct {
	manager    session.Service
	signingKey func(context.Context) (ed25519.PrivateKey, error)
	mu         sync.Mutex
	// carriers opens a carrier's output stream; the empty name is SetPublisher's single carrier.
	carriers map[string]OpenPublisher
	sessions map[publicationKey]*publication
}

type publicationKey struct{ sessionID, carrier string }

type publication struct {
	carrier       string
	sessionID     string
	generation    uint64
	attachmentID  string
	epoch         Epoch
	index         uint64
	subscribers   map[string]chan Epoch
	stream        io.WriteCloser
	cancel        context.CancelFunc
	diagnosticCtx context.Context
	compress      protocol.CompressionGate // backs compression off while the session's output does not compress
	mu            sync.Mutex
	// writeMu orders records on the stream. Writes happen outside mu, so a stalled stream never blocks
	// Join or Leave (Leave's stop closes the stream, which ends the stalled write).
	writeMu  sync.Mutex
	stopOnce sync.Once
	// lags counts the output dropped behind a flood. A drop is silent until a later record shows the gap, so a
	// flood that ends inside one would leave browsers on a stale screen; their streams send one on a change.
	lags atomic.Uint64
}

func NewRegistry(manager session.Service, signingKey func(context.Context) (ed25519.PrivateKey, error)) (*Registry, error) {
	if manager == nil || signingKey == nil {
		return nil, ErrUnavailable
	}
	return &Registry{manager: manager, signingKey: signingKey, carriers: make(map[string]OpenPublisher), sessions: make(map[publicationKey]*publication)}, nil
}

// SetPublisher replaces every carrier with one unnamed output stream opener, or with none. Existing publications
// close, which makes browsers reconnect and obtain a fresh screen checkpoint and epoch.
func (r *Registry) SetPublisher(open OpenPublisher) {
	r.mu.Lock()
	r.carriers = make(map[string]OpenPublisher)
	if open != nil {
		r.carriers[""] = open
	}
	old := r.sessions
	r.sessions = make(map[publicationKey]*publication)
	r.mu.Unlock()
	for _, p := range old {
		r.stop(p)
	}
}

// SetCarrier adds one runtime carrier's output stream opener, or removes the carrier when open is nil. Only that
// carrier's publications close (their browsers reconnect); browsers on other carriers keep their output.
func (r *Registry) SetCarrier(carrier string, open OpenPublisher) {
	r.mu.Lock()
	if open == nil {
		delete(r.carriers, carrier)
	} else {
		r.carriers[carrier] = open
	}
	var old []*publication
	for key, p := range r.sessions {
		if key.carrier == carrier {
			old = append(old, p)
			delete(r.sessions, key)
		}
	}
	r.mu.Unlock()
	for _, p := range old {
		r.stop(p)
	}
}

// Join returns an initial key update and later rotations for one authenticated
// browser attachment, which arrived on the named carrier. The caller must close the stream with Leave.
func (r *Registry) Join(ctx context.Context, sessionID string, generation uint64, attachmentID, carrier string) (<-chan Epoch, error) {
	if r == nil || ctx == nil || sessionID == "" || generation == 0 || attachmentID == "" {
		return nil, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	open := r.carriers[carrier]
	if open == nil {
		return nil, ErrUnavailable
	}
	joined := 0
	for key, p := range r.sessions {
		if key.sessionID != sessionID {
			continue
		}
		p.mu.Lock()
		joined += len(p.subscribers)
		taken := p.subscribers[attachmentID] != nil
		p.mu.Unlock()
		if taken {
			return nil, ErrUnavailable
		}
	}
	if joined >= maxSubscribers {
		return nil, ErrUnavailable
	}
	key := publicationKey{sessionID, carrier}
	if p := r.sessions[key]; p != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.generation != generation {
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
	signing, err := r.signingKey(ctx)
	if err != nil {
		return nil, classifiedFailure(err, streamOpenStage, sessionCode)
	}
	if len(signing) != ed25519.PrivateKeySize {
		return nil, ErrUnavailable
	}
	workerKey = append(ed25519.PrivateKey(nil), signing...)
	epoch, err := browserbroadcast.NewEpoch()
	if err != nil {
		return nil, classifiedFailure(err, streamOpenStage, sessionCode)
	}
	diagnosticCtx := context.WithoutCancel(ctx)
	if !supportref.Valid(supportref.FromContext(diagnosticCtx)) {
		diagnosticCtx = supportref.WithContext(diagnosticCtx, supportref.New())
	}
	runCtx, cancel := context.WithCancel(diagnosticCtx)
	// The stream lives as long as the publication, not the joining browser's request: the carrier resets a
	// stream whose open context ends, which dropped every browser 10 s after the first one attached.
	publisher, err := open(runCtx, sessionID)
	if err != nil {
		cancel()
		return nil, classifiedFailure(err, streamOpenStage, transportCode)
	}
	producerAttachment := "broadcast_" + hex.EncodeToString(epoch.ID[:])
	if _, err = r.manager.AttachLive(sessionID, producerAttachment); err != nil {
		cancel()
		return nil, classifiedFailure(errors.Join(err, publisher.Close()), streamOpenStage, sessionCode)
	}
	updates := make(chan Epoch, 1)
	updates <- epoch
	p := &publication{carrier: carrier, sessionID: sessionID, generation: generation, attachmentID: producerAttachment, epoch: epoch, subscribers: map[string]chan Epoch{attachmentID: updates}, stream: publisher, cancel: cancel, diagnosticCtx: diagnosticCtx}
	r.sessions[key] = p
	go r.publish(runCtx, p, workerKey)
	workerKey = nil
	return updates, nil
}

// Attached reports how many browser attachments receive this registry's output now.
func (r *Registry) Attached() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, p := range r.sessions {
		p.mu.Lock()
		count += len(p.subscribers)
		p.mu.Unlock()
	}
	return count
}

// Lags reports how often the shared output on the attachment's carrier dropped a backlog (see publication.lags).
func (r *Registry) Lags(sessionID, attachmentID string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, _ := r.holderLocked(sessionID, attachmentID); p != nil {
		return p.lags.Load()
	}
	return 0
}

// holderLocked finds the session's publication that feeds the attachment. The caller holds r.mu.
func (r *Registry) holderLocked(sessionID, attachmentID string) (*publication, publicationKey) {
	for key, p := range r.sessions {
		if key.sessionID != sessionID {
			continue
		}
		p.mu.Lock()
		held := p.subscribers[attachmentID] != nil
		p.mu.Unlock()
		if held {
			return p, key
		}
	}
	return nil, publicationKey{}
}

func (r *Registry) Leave(sessionID, attachmentID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	p, key := r.holderLocked(sessionID, attachmentID)
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
		delete(r.sessions, key)
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
		delete(r.sessions, key)
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
		delete(r.sessions, key)
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
		diagnosticlog.TryInfo("browser output publisher stopped")
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
		if key := (publicationKey{p.sessionID, p.carrier}); r.sessions[key] == p {
			delete(r.sessions, key)
		}
		r.mu.Unlock()
		r.stop(p)
	}()
	var received uint64 // end of the last event sealed: the owner frees output only up to here
	for {
		event, err := r.manager.WaitNext(ctx, p.sessionID, p.attachmentID, received)
		if errors.Is(err, session.ErrAttachmentLagged) {
			p.lags.Add(1)
			continue // records skip ahead; each browser sees the gap and restores its screen in-band
		}
		if err != nil {
			diagnosticlog.TryInfo("browser output publisher ended: waiting for output failed")
			reportPublisherFailure(p.diagnosticCtx, deliveryStage, err)
			return
		}
		p.writeMu.Lock()
		p.mu.Lock()
		if p.index == ^uint64(0) {
			p.mu.Unlock()
			p.writeMu.Unlock()
			event.Release()
			reportPublisherFailure(p.diagnosticCtx, deliveryStage, errOutputIndexExhausted)
			return
		}
		p.index++
		frame, encodeErr := browserbroadcast.Seal(browserbroadcast.Record{SessionID: p.sessionID, Generation: p.generation, EpochID: p.epoch.ID, Index: p.index, Channel: byte(event.Channel), StartSequence: event.StartSequence, Data: event.Data}, p.epoch, key, &p.compress)
		p.mu.Unlock()
		if encodeErr == nil {
			encodeErr = writeRecord(p.stream, frame)
		}
		p.writeMu.Unlock()
		received = event.EndSequence
		event.Release()
		if encodeErr != nil {
			diagnosticlog.TryInfo("browser output publisher ended: writing to the edge failed")
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

// writeRecord has no write deadline: a congested carrier blocks the publisher and the browsers lag in-band (the
// fanout drops the backlog); a dead carrier resets the stream, which fails the write.
func writeRecord(writer io.Writer, payload []byte) error {
	if len(payload) == 0 || len(payload) > browserbroadcast.MaxRecordBytes {
		return browserbroadcast.ErrInvalidRecord
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	written, err := writer.Write(header[:])
	if err != nil {
		return err
	}
	if written != len(header) {
		return io.ErrShortWrite
	}
	for len(payload) != 0 {
		n, err := writer.Write(payload)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(payload) {
			return io.ErrShortWrite
		}
		payload = payload[n:]
	}
	return nil
}
