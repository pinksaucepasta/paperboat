package edgehttp

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
)

const (
	browserTerminalMaxParticipants = 100
	browserTerminalMaxSessions     = 4096
	browserTerminalMaxRecordBytes  = (128 << 10) + 1024
	browserTerminalQueuedRecords   = 256
	browserTerminalQueuedBytes     = 512 << 10
)

var (
	ErrBrowserTerminalHubInvalid = errors.New("invalid browser terminal fanout request")
	ErrBrowserTerminalHubClosed  = errors.New("browser terminal fanout is closed")
	ErrBrowserTerminalHubStale   = errors.New("browser terminal carrier generation is stale")
	ErrBrowserTerminalHubFull    = errors.New("browser terminal participant limit reached")
	ErrBrowserTerminalHubBusy    = errors.New("browser terminal output publisher already exists")
	ErrBrowserTerminalHubEnded   = errors.New("browser terminal participant ended")
)

// BrowserTerminalSessionKey binds opaque shared output to one route, host
// terminal session, and authenticated carrier process generation.
type BrowserTerminalSessionKey struct {
	RouteID           string
	TerminalSessionID string
	ProcessGeneration uint64
}

// BrowserTerminalRouteFence is the current admitted runtime attachment for a
// route. It keeps a delayed detach from an older carrier from evicting a new
// attachment that reused the same process generation.
type BrowserTerminalRouteFence struct {
	Identity             datacarrier.Identity
	Revision             uint64
	AttachmentGeneration uint64
}

type browserTerminalSession struct {
	publisher   *BrowserTerminalPublisher
	subscribers map[string]*BrowserTerminalSubscription
}

// BrowserTerminalHub forwards complete opaque output records without reading,
// retaining, or interpreting their contents. Each participant has a bounded
// burst buffer; a participant that stays behind is disconnected independently.
type BrowserTerminalHub struct {
	mu           sync.Mutex
	closed       bool
	activeRoutes map[string]BrowserTerminalRouteFence
	sessions     map[BrowserTerminalSessionKey]*browserTerminalSession
}

func NewBrowserTerminalHub() *BrowserTerminalHub {
	return &BrowserTerminalHub{activeRoutes: make(map[string]BrowserTerminalRouteFence), sessions: make(map[BrowserTerminalSessionKey]*browserTerminalSession)}
}

// ActivateRoute records the current runtime process generation. A newer
// generation fences and closes old publishers and participant subscriptions.
func (h *BrowserTerminalHub) ActivateRoute(routeID string, fence BrowserTerminalRouteFence) error {
	if h == nil || connectorprotocol.ValidateIdentifier(routeID) != nil || fence.Identity.Validate() != nil || fence.Revision == 0 || fence.AttachmentGeneration == 0 {
		return ErrBrowserTerminalHubInvalid
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrBrowserTerminalHubClosed
	}
	if current, ok := h.activeRoutes[routeID]; ok && current != fence {
		h.activeRoutes[routeID] = fence
		for key, session := range h.sessions {
			if key.RouteID == routeID {
				h.closeSessionLocked(key, session)
			}
		}
		return nil
	}
	h.activeRoutes[routeID] = fence
	return nil
}

// DeactivateRoute closes only the generation that still owns the route. A
// delayed cleanup from an older carrier cannot evict its replacement.
func (h *BrowserTerminalHub) DeactivateRoute(routeID string, fence BrowserTerminalRouteFence) {
	if h == nil || routeID == "" || fence.Identity.Validate() != nil || fence.Revision == 0 || fence.AttachmentGeneration == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.activeRoutes[routeID] != fence {
		return
	}
	delete(h.activeRoutes, routeID)
	for key, session := range h.sessions {
		if key.RouteID == routeID {
			h.closeSessionLocked(key, session)
		}
	}
}

// Subscribe admits one authorized browser attachment to the shared output
// stream. The admission is bound to the route generation selected by policy.
func (h *BrowserTerminalHub) Subscribe(key BrowserTerminalSessionKey, fence BrowserTerminalRouteFence, attachmentID string) (*BrowserTerminalSubscription, error) {
	if !validBrowserTerminalSessionKey(key) || !validBrowserTerminalRouteFence(key, fence) || connectorprotocol.ValidateIdentifier(attachmentID) != nil {
		return nil, ErrBrowserTerminalHubInvalid
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrBrowserTerminalHubClosed
	}
	if h.activeRoutes[key.RouteID] != fence {
		return nil, ErrBrowserTerminalHubStale
	}
	session := h.sessions[key]
	if session == nil {
		if len(h.sessions) >= browserTerminalMaxSessions {
			return nil, ErrBrowserTerminalHubFull
		}
		session = &browserTerminalSession{subscribers: make(map[string]*BrowserTerminalSubscription)}
		h.sessions[key] = session
	}
	if _, exists := session.subscribers[attachmentID]; exists {
		return nil, ErrBrowserTerminalHubInvalid
	}
	if len(session.subscribers) >= browserTerminalMaxParticipants {
		return nil, ErrBrowserTerminalHubFull
	}
	subscription := &BrowserTerminalSubscription{hub: h, key: key, attachmentID: attachmentID, records: make(chan []byte, browserTerminalQueuedRecords), done: make(chan struct{})}
	session.subscribers[attachmentID] = subscription
	return subscription, nil
}

// RegisterPublisher reserves the single host output stream for this terminal
// generation. Duplicate publishers and streams from fenced generations fail.
func (h *BrowserTerminalHub) RegisterPublisher(key BrowserTerminalSessionKey, identity datacarrier.Identity) (*BrowserTerminalPublisher, error) {
	if !validBrowserTerminalSessionKey(key) || identity.Validate() != nil || key.ProcessGeneration != identity.ProcessGeneration {
		return nil, ErrBrowserTerminalHubInvalid
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrBrowserTerminalHubClosed
	}
	fence, ok := h.activeRoutes[key.RouteID]
	if !ok || fence.Identity != identity {
		return nil, ErrBrowserTerminalHubStale
	}
	session := h.sessions[key]
	if session == nil {
		if len(h.sessions) >= browserTerminalMaxSessions {
			return nil, ErrBrowserTerminalHubFull
		}
		session = &browserTerminalSession{subscribers: make(map[string]*BrowserTerminalSubscription)}
		h.sessions[key] = session
	}
	if session.publisher != nil {
		return nil, ErrBrowserTerminalHubBusy
	}
	publisher := &BrowserTerminalPublisher{hub: h, key: key, identity: identity, done: make(chan struct{})}
	session.publisher = publisher
	return publisher, nil
}

// ServeCarrier accepts only the dedicated host-initiated output stream kind.
// Other or malformed inbound streams are closed by datacarrier and ignored;
// this loop never treats them as terminal output.
func (h *BrowserTerminalHub) ServeCarrier(ctx context.Context, server *datacarrier.Server) error {
	if h == nil || ctx == nil || server == nil {
		return ErrBrowserTerminalHubInvalid
	}
	for {
		stream, open, err := server.AcceptBrowserTerminalOutputStream(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, datacarrier.ErrCarrierClosed) {
				return nil
			}
			if errors.Is(err, datacarrier.ErrBrowserTerminalOutputKind) || errors.Is(err, datacarrier.ErrAccessStreamIdentity) || errors.Is(err, datacarrier.ErrRouteDenied) || errors.Is(err, datacarrier.ErrInvalidPreface) || errors.Is(err, connectorprotocol.ErrMalformedFrame) || errors.Is(err, connectorprotocol.ErrFrameTooLarge) {
				continue
			}
			return err
		}
		key := BrowserTerminalSessionKey{RouteID: open.RouteID, TerminalSessionID: open.RequestID, ProcessGeneration: open.ProcessGeneration}
		publisher, err := h.RegisterPublisher(key, server.Identity())
		if err != nil {
			_ = stream.Close()
			continue
		}
		go h.servePublisher(ctx, stream, publisher)
	}
}

func (h *BrowserTerminalHub) servePublisher(parent context.Context, stream *datacarrier.Stream, publisher *BrowserTerminalPublisher) {
	defer stream.Close()
	defer publisher.Close()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stopClose := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stopClose()
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-publisher.Done():
			cancel()
		case <-ctx.Done():
		case <-watchDone:
		}
	}()
	for {
		record, err := readBrowserTerminalRecord(stream)
		if err != nil {
			return
		}
		if err := publisher.Publish(record); err != nil {
			return
		}
	}
}

func (h *BrowserTerminalHub) unsubscribe(subscription *BrowserTerminalSubscription) {
	if h == nil || subscription == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	session := h.sessions[subscription.key]
	if session == nil || session.subscribers[subscription.attachmentID] != subscription {
		return
	}
	delete(session.subscribers, subscription.attachmentID)
	subscription.closeDone()
	h.cleanupSessionLocked(subscription.key, session)
}

func (h *BrowserTerminalHub) unregisterPublisher(publisher *BrowserTerminalPublisher) {
	if h == nil || publisher == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	session := h.sessions[publisher.key]
	if session == nil || session.publisher != publisher {
		return
	}
	session.publisher = nil
	publisher.closeDone()
	// A live browser cannot continue on a route whose only encrypted output
	// feed ended. Closing its subscription forces a fresh ticket and screen
	// checkpoint, including when the carrier fails without an edge restart.
	h.closeSessionLocked(publisher.key, session)
}

func (h *BrowserTerminalHub) closeSessionLocked(key BrowserTerminalSessionKey, session *browserTerminalSession) {
	if session.publisher != nil {
		session.publisher.closeDone()
		session.publisher = nil
	}
	for attachmentID, subscription := range session.subscribers {
		delete(session.subscribers, attachmentID)
		subscription.closeDone()
	}
	delete(h.sessions, key)
}

func (h *BrowserTerminalHub) cleanupSessionLocked(key BrowserTerminalSessionKey, session *browserTerminalSession) {
	if session.publisher == nil && len(session.subscribers) == 0 {
		delete(h.sessions, key)
	}
}

// Close ends every participant and publisher and releases all in-memory
// session state. It is safe to call repeatedly.
func (h *BrowserTerminalHub) Close() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	for key, session := range h.sessions {
		h.closeSessionLocked(key, session)
	}
	h.activeRoutes = make(map[string]BrowserTerminalRouteFence)
	return nil
}

// BrowserTerminalSubscription owns one admitted browser participant.
type BrowserTerminalSubscription struct {
	hub          *BrowserTerminalHub
	key          BrowserTerminalSessionKey
	attachmentID string
	records      chan []byte
	queuedBytes  int // guarded by hub.mu
	done         chan struct{}
	once         sync.Once
	doneOnce     sync.Once
}

func (s *BrowserTerminalSubscription) Next(ctx context.Context) ([]byte, error) {
	if s == nil || ctx == nil {
		return nil, ErrBrowserTerminalHubInvalid
	}
	select {
	case <-s.done:
		return nil, ErrBrowserTerminalHubEnded
	default:
	}
	select {
	case <-s.done:
		return nil, ErrBrowserTerminalHubEnded
	case <-ctx.Done():
		return nil, ctx.Err()
	case record := <-s.records:
		s.hub.mu.Lock()
		s.queuedBytes -= len(record)
		s.hub.mu.Unlock()
		select {
		case <-s.done:
			return nil, ErrBrowserTerminalHubEnded
		default:
			return record, nil
		}
	}
}

func (s *BrowserTerminalSubscription) Done() <-chan struct{} {
	if s == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return s.done
}

func (s *BrowserTerminalSubscription) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() { s.hub.unsubscribe(s) })
}

func (s *BrowserTerminalSubscription) closeDone() { s.doneOnce.Do(func() { close(s.done) }) }

// BrowserTerminalPublisher owns the only host output stream for one terminal
// generation.
type BrowserTerminalPublisher struct {
	hub      *BrowserTerminalHub
	key      BrowserTerminalSessionKey
	identity datacarrier.Identity
	done     chan struct{}
	once     sync.Once
	doneOnce sync.Once
}

func (p *BrowserTerminalPublisher) Publish(record []byte) error {
	if p == nil || p.hub == nil || len(record) == 0 || len(record) > browserTerminalMaxRecordBytes {
		return ErrBrowserTerminalHubInvalid
	}
	h := p.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-p.done:
		return ErrBrowserTerminalHubEnded
	default:
	}
	fence, active := h.activeRoutes[p.key.RouteID]
	if h.closed || !active || fence.Identity != p.identity || p.key.ProcessGeneration != p.identity.ProcessGeneration {
		return ErrBrowserTerminalHubStale
	}
	session := h.sessions[p.key]
	if session == nil || session.publisher != p {
		return ErrBrowserTerminalHubStale
	}
	for attachmentID, subscription := range session.subscribers {
		if subscription.queuedBytes+len(record) > browserTerminalQueuedBytes {
			delete(session.subscribers, attachmentID)
			subscription.closeDone()
			continue
		}
		select {
		case subscription.records <- record:
			subscription.queuedBytes += len(record)
		default:
			delete(session.subscribers, attachmentID)
			subscription.closeDone()
		}
	}
	h.cleanupSessionLocked(p.key, session)
	return nil
}

func (p *BrowserTerminalPublisher) Done() <-chan struct{} {
	if p == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return p.done
}

func (p *BrowserTerminalPublisher) Close() {
	if p == nil || p.hub == nil {
		return
	}
	p.once.Do(func() { p.hub.unregisterPublisher(p) })
}

func (p *BrowserTerminalPublisher) closeDone() { p.doneOnce.Do(func() { close(p.done) }) }

func validBrowserTerminalSessionKey(key BrowserTerminalSessionKey) bool {
	return connectorprotocol.ValidateIdentifier(key.RouteID) == nil && connectorprotocol.ValidateIdentifier(key.TerminalSessionID) == nil && key.ProcessGeneration != 0
}

func validBrowserTerminalRouteFence(key BrowserTerminalSessionKey, fence BrowserTerminalRouteFence) bool {
	return fence.Identity.Validate() == nil && fence.Identity.ProcessGeneration == key.ProcessGeneration && fence.Revision != 0 && fence.AttachmentGeneration != 0
}

func readBrowserTerminalRecord(reader io.Reader) ([]byte, error) {
	var header [4]byte
	n, err := io.ReadFull(reader, header[:])
	if err != nil {
		if err == io.EOF && n == 0 {
			return nil, io.EOF
		}
		return nil, err
	}
	length := int(binary.BigEndian.Uint32(header[:]))
	if length == 0 || length > browserTerminalMaxRecordBytes {
		return nil, ErrBrowserTerminalHubInvalid
	}
	record := make([]byte, length)
	if _, err := io.ReadFull(reader, record); err != nil {
		return nil, err
	}
	return record, nil
}
