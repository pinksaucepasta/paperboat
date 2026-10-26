package connector

import (
	"context"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

func NewDataCarrierClient(ctx context.Context, link io.ReadWriteCloser, config DataCarrierConfig, identity ...DataCarrierIdentity) (*DataCarrier, error) {
	if len(identity) > 1 {
		return nil, ErrInvalidDataCarrierConfig
	}
	var bound DataCarrierIdentity
	if len(identity) == 1 {
		bound = identity[0]
		if err := bound.validate(); err != nil {
			return nil, err
		}
	}
	return newDataCarrier(ctx, link, config, true, bound, nil)
}

// NewDataCarrierClientWithSession constructs a client on a transport-native
// session, such as a QUIC connection. The session owns all stream creation,
// acceptance, keepalive, and closure semantics.
func NewDataCarrierClientWithSession(ctx context.Context, session DataCarrierSession, config DataCarrierConfig, identity ...DataCarrierIdentity) (*DataCarrier, error) {
	if len(identity) > 1 {
		return nil, ErrInvalidDataCarrierConfig
	}
	var bound DataCarrierIdentity
	if len(identity) == 1 {
		bound = identity[0]
		if err := bound.validate(); err != nil {
			return nil, err
		}
	}
	return newDataCarrierWithSession(ctx, session, config, bound, nil)
}

func NewDataCarrierServer(ctx context.Context, link io.ReadWriteCloser, config DataCarrierConfig, admission ...DataCarrierAdmission) (*DataCarrier, error) {
	if len(admission) != 1 {
		return nil, ErrInvalidDataCarrierConfig
	}
	var bound *DataCarrierAdmission
	if len(admission) == 1 {
		if err := admission[0].validate(); err != nil {
			return nil, err
		}
		bound = &admission[0]
	}
	return newDataCarrier(ctx, link, config, false, DataCarrierIdentity{}, bound)
}

// NewDataCarrierServerWithSession constructs a server on a transport-native
// session. Admission remains mandatory so an authenticated control-session
// identity and route owner are always present before bytes are exposed.
func NewDataCarrierServerWithSession(ctx context.Context, session DataCarrierSession, config DataCarrierConfig, admission ...DataCarrierAdmission) (*DataCarrier, error) {
	if len(admission) != 1 || session == nil {
		return nil, ErrInvalidDataCarrierConfig
	}
	if err := admission[0].validate(); err != nil {
		return nil, err
	}
	return newDataCarrierWithSession(ctx, session, config, DataCarrierIdentity{}, &admission[0])
}

func newDataCarrier(ctx context.Context, link io.ReadWriteCloser, config DataCarrierConfig, client bool, identity DataCarrierIdentity, admission *DataCarrierAdmission) (*DataCarrier, error) {
	if ctx == nil || link == nil {
		return nil, ErrInvalidDataCarrierConfig
	}
	config = config.withDefaults()
	if err := config.Validate(); err != nil {
		return nil, err
	}
	session, err := newYamuxDataCarrierSession(link, config, client)
	if err != nil {
		_ = link.Close()
		return nil, err
	}
	return newDataCarrierWithSession(ctx, session, config, identity, admission)
}

func newDataCarrierWithSession(ctx context.Context, session DataCarrierSession, config DataCarrierConfig, identity DataCarrierIdentity, admission *DataCarrierAdmission) (*DataCarrier, error) {
	if ctx == nil || session == nil {
		return nil, ErrInvalidDataCarrierConfig
	}
	config = config.withDefaults()
	if err := config.Validate(); err != nil {
		return nil, err
	}
	c := &DataCarrier{
		id:        "carrier_" + uuid.NewString(),
		ctx:       ctx,
		session:   session,
		config:    config,
		identity:  identity,
		admission: admission,
		permits:   make(chan struct{}, config.MaximumStreams),
		openings:  make(chan struct{}, config.QueueDepth),
		accepted:  make(chan dataCarrierAcceptResult, config.QueueDepth),
		done:      make(chan struct{}),
		started:   time.Now(),
	}
	go c.watch(ctx)
	go c.acceptLoop()
	return c, nil
}

func (c *DataCarrier) watch(ctx context.Context) {
	select {
	case <-ctx.Done():
		c.shutdownFor(CloseContextDone, ctx.Err())
	case <-c.session.CloseChan():
		c.shutdownFor(CloseSessionClosed, ErrDataCarrierClosed)
	}
}

func (c *DataCarrier) acceptLoop() {
	defer close(c.accepted)
	for {
		raw, err := c.session.AcceptStream(c.ctx)
		if err != nil {
			if !c.closed() {
				reason := CloseSessionError
				if c.ctx.Err() != nil {
					reason = CloseContextDone
				}
				c.shutdownFor(reason, err)
			}
			return
		}
		select {
		case c.permits <- struct{}{}:
			c.active.Add(1)
		case <-c.done:
			_ = raw.Close()
			return
		}
		var open StreamOpen
		if c.admission != nil {
			admissionContext, cancel := context.WithTimeout(c.ctx, c.config.StreamOpenLimit)
			deadline := time.Now().Add(c.config.StreamOpenLimit)
			if err := raw.SetReadDeadline(deadline); err != nil {
				cancel()
				_ = raw.Close()
				c.releasePermit()
				c.publishAccepted(dataCarrierAcceptResult{err: wrapDataCarrierStreamOpenFailure(err, ErrDataCarrierAdmission)})
				continue
			}
			open, err = connectorprotocol.ReadStreamOpen(raw)
			_ = raw.SetReadDeadline(time.Time{})
			if err != nil {
				err = wrapDataCarrierStreamOpenFailure(err, nil)
			}
			if err == nil && !c.admission.Identity.matches(open) {
				err = &dataCarrierAdmissionFailure{cause: ErrDataCarrierAdmission}
			}
			if err == nil {
				err = c.admission.Authorize(admissionContext, open)
				if err != nil {
					err = &dataCarrierAdmissionFailure{cause: err}
				}
			}
			cancel()
			if err != nil {
				_ = raw.Close()
				c.releasePermit()
				c.publishAccepted(dataCarrierAcceptResult{err: err})
				continue
			}
		}
		stream := c.wrapStream(raw, context.Background(), nil)
		stream.Open = open
		select {
		case c.accepted <- dataCarrierAcceptResult{stream: stream, open: open}:
		case <-c.done:
			_ = stream.Close()
			return
		}
	}
}

func (c *DataCarrier) publishAccepted(result dataCarrierAcceptResult) {
	select {
	case c.accepted <- result:
	case <-c.done:
		if result.stream != nil {
			_ = result.stream.Close()
		}
	}
}

func (c *DataCarrier) ID() string {
	if c == nil {
		return ""
	}
	return c.id
}

// Identity returns the authenticated control-session identity bound to this
// carrier. HostID is intentionally local metadata and is never repeated in
// the per-stream wire preface.
func (c *DataCarrier) Identity() DataCarrierIdentity {
	if c == nil {
		return DataCarrierIdentity{}
	}
	return c.identity
}

func (c *DataCarrier) State() DataCarrierState {
	if c == nil || c.closed() {
		return DataCarrierClosed
	}
	return DataCarrierReady
}

func (c *DataCarrier) ActiveStreams() int {
	if c == nil || c.closed() {
		return 0
	}
	active := c.active.Load()
	if active <= 0 {
		return 0
	}
	return int(active)
}

func (c *DataCarrier) Done() <-chan struct{} {
	if c == nil {
		return closedDataCarrierChannel()
	}
	return c.done
}

func (c *DataCarrier) Ping(ctx context.Context) error {
	if c == nil || c.session == nil || ctx == nil {
		return ErrInvalidDataCarrierConfig
	}
	if c.closed() {
		return ErrDataCarrierClosed
	}
	err := c.session.Ping(ctx)
	if err != nil && c.closed() {
		return ErrDataCarrierClosed
	}
	return err
}

func (c *DataCarrier) OpenStream(ctx context.Context, open ...StreamOpen) (io.ReadWriteCloser, error) {
	if len(open) > 1 {
		return nil, ErrInvalidDataCarrierConfig
	}
	var metadata *StreamOpen
	if len(open) == 1 {
		if err := open[0].Validate(); err != nil {
			return nil, err
		}
		if c == nil || c.identity.AccountID == "" || !c.identity.matches(open[0]) {
			return nil, ErrDataCarrierAdmission
		}
		metadata = &open[0]
	}
	return c.openStreamWithMetadata(ctx, nil, metadata)
}

// OpenControlStream reserves one ordinary carrier stream for the already
// authenticated connector-v1 control session.  Control frames are owned by
// the protocol package and therefore have no data-stream preface.
func (c *DataCarrier) OpenControlStream(ctx context.Context) (io.ReadWriteCloser, error) {
	return c.openStream(ctx, nil)
}

func (c *DataCarrier) openStream(ctx context.Context, onClose func()) (io.ReadWriteCloser, error) {
	return c.openStreamWithMetadata(ctx, onClose, nil)
}

func (c *DataCarrier) openStreamWithMetadata(ctx context.Context, onClose func(), open *StreamOpen) (io.ReadWriteCloser, error) {
	if c == nil || c.session == nil || ctx == nil {
		return nil, ErrInvalidDataCarrierConfig
	}
	if open != nil {
		if err := open.Validate(); err != nil {
			return nil, err
		}
		if c.identity.AccountID == "" || !c.identity.matches(*open) {
			return nil, ErrDataCarrierAdmission
		}
	}
	if c.closed() {
		return nil, ErrDataCarrierClosed
	}
	select {
	case c.permits <- struct{}{}:
		c.active.Add(1)
	case <-c.done:
		return nil, ErrDataCarrierClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, ErrDataCarrierLimit
	}
	select {
	case c.openings <- struct{}{}:
	case <-c.done:
		c.releasePermit()
		return nil, ErrDataCarrierClosed
	case <-ctx.Done():
		c.releasePermit()
		return nil, ctx.Err()
	}
	result := make(chan dataCarrierOpenResult, 1)
	go func() {
		raw, err := c.session.OpenStream(ctx)
		result <- dataCarrierOpenResult{stream: raw, err: err}
	}()
	select {
	case opened := <-result:
		<-c.openings
		if opened.err != nil {
			c.releasePermit()
			if c.closed() {
				return nil, ErrDataCarrierClosed
			}
			return nil, wrapDataCarrierStreamOpenFailure(opened.err, nil)
		}
		if err := ctx.Err(); err != nil {
			_ = opened.stream.Close()
			c.releasePermit()
			return nil, err
		}
		if open != nil {
			if err := writeDataCarrierStreamOpen(ctx, opened.stream, *open, c.config.StreamOpenLimit); err != nil {
				_ = opened.stream.Close()
				c.releasePermit()
				return nil, wrapDataCarrierStreamOpenFailure(err, nil)
			}
		}
		if open != nil {
			stream := c.wrapStream(opened.stream, ctx, onClose, *open)
			return stream, nil
		}
		stream := c.wrapStream(opened.stream, ctx, onClose)
		return stream, nil
	case <-c.done:
		go c.finishCanceledOpen(result)
		return nil, ErrDataCarrierClosed
	case <-ctx.Done():
		go c.finishCanceledOpen(result)
		return nil, ctx.Err()
	}
}

type dataCarrierOpenResult struct {
	stream DataCarrierStreamLink
	err    error
}

func (c *DataCarrier) finishCanceledOpen(result <-chan dataCarrierOpenResult) {
	opened := <-result
	<-c.openings
	if opened.err == nil && opened.stream != nil {
		_ = opened.stream.Close()
	}
	c.releasePermit()
}

func (c *DataCarrier) AcceptStream(ctx context.Context) (*DataCarrierStream, StreamOpen, error) {
	if c == nil || ctx == nil {
		return nil, StreamOpen{}, ErrInvalidDataCarrierConfig
	}
	if c.closed() {
		return nil, StreamOpen{}, ErrDataCarrierClosed
	}
	select {
	case result, ok := <-c.accepted:
		if !ok {
			return nil, StreamOpen{}, ErrDataCarrierClosed
		}
		return result.stream, result.open, result.err
	case <-c.done:
		return nil, StreamOpen{}, ErrDataCarrierClosed
	case <-ctx.Done():
		return nil, StreamOpen{}, ctx.Err()
	}
}

func writeDataCarrierStreamOpen(ctx context.Context, stream DataCarrierStreamLink, open StreamOpen, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := stream.SetWriteDeadline(deadline); err != nil {
		return err
	}
	err := connectorprotocol.WriteStreamOpen(stream, open)
	_ = stream.SetWriteDeadline(time.Time{})
	if err != nil {
		return err
	}
	return ctx.Err()
}

func (c *DataCarrier) wrapStream(raw DataCarrierStreamLink, ctx context.Context, onClose func(), open ...StreamOpen) *DataCarrierStream {
	stream := &DataCarrierStream{raw: raw, release: c.releasePermit, onClose: onClose}
	if len(open) > 0 {
		stream.Open = open[0]
	}
	if ctx != nil && ctx != context.Background() {
		stream.cancelMu.Lock()
		stream.stopCancel = context.AfterFunc(ctx, func() { _ = stream.abort() })
		stream.cancelMu.Unlock()
	}
	return stream
}

func (c *DataCarrier) releasePermit() {
	if !c.closed() {
		c.active.Add(-1)
	}
	select {
	case <-c.permits:
	default:
	}
}

func (c *DataCarrier) shutdown(err error) { c.shutdownFor(CloseLocal, err) }

// shutdownFor ends the carrier once; the first reason wins and is logged with the streams that
// were still open, which are the ones the close cut.
func (c *DataCarrier) shutdownFor(reason DataCarrierCloseReason, err error) {
	c.closeOnce.Do(func() {
		c.closeErr = err
		c.reason = reason
		openStreams := c.active.Load()
		close(c.done)
		_ = c.session.Close()
		age := time.Duration(0)
		if !c.started.IsZero() {
			age = time.Since(c.started).Round(time.Second)
		}
		diagnosticlog.TryWarn("data carrier closed", "reason", string(reason), "open_streams", openStreams, "age", age.String())
	})
}

// CloseReason returns why the carrier ended, or "" while it is open.
func (c *DataCarrier) CloseReason() DataCarrierCloseReason {
	if c == nil || !c.closed() {
		return ""
	}
	return c.reason
}

func (c *DataCarrier) Close() error {
	if c == nil {
		return nil
	}
	c.shutdown(nil)
	return c.closeErr
}

// closeFor closes the carrier and records the pool's reason.
func (c *DataCarrier) closeFor(reason DataCarrierCloseReason) error {
	if c == nil {
		return nil
	}
	c.shutdownFor(reason, nil)
	return c.closeErr
}

func (c *DataCarrier) closed() bool {
	if c == nil || c.done == nil {
		return true
	}
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func closedDataCarrierChannel() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
