package connector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"sync"
	"time"
)

type DataCarrierPool struct {
	ctx    context.Context
	cancel context.CancelFunc
	dial   DataCarrierDialer
	config DataCarrierPoolConfig
	queue  chan struct{}
	done   chan struct{}
	ready  chan struct{}

	connectMu      sync.Mutex
	mu             sync.RWMutex
	state          DataCarrierPoolState
	selected       Transport
	carriers       []*pooledDataCarrier
	readyOnce      sync.Once
	closeOnce      sync.Once
	control        *DataCarrierStream
	controlOpening bool
	accepted       chan dataCarrierAcceptResult
	workers        sync.WaitGroup
	repairOnce     sync.Once
	refreshedAt    time.Time
	// Placement fields are owned exclusively by connectMu; transport config is immutable.
	targets         []DataCarrierTarget
	targetExpiry    time.Time
	maximumCarriers int
	retries         map[int]carrierSlotRetry
	fallbackUntil   time.Time
}

// Identity returns a copy of the exact authenticated session binding used by
// every carrier in this pool. It never exposes credential material.
func (p *DataCarrierPool) Identity() (DataCarrierIdentity, bool) {
	if p == nil {
		return DataCarrierIdentity{}, false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.state == DataCarrierPoolClosed || p.config.Session.validate() != nil {
		return DataCarrierIdentity{}, false
	}
	return p.config.Session, true
}

func NewDataCarrierPool(ctx context.Context, config DataCarrierPoolConfig, dialer DataCarrierDialer) (*DataCarrierPool, error) {
	if ctx == nil || dialer == nil {
		return nil, ErrInvalidDataCarrierConfig
	}
	config = config.withDefaults()
	if err := config.Validate(); err != nil {
		return nil, err
	}
	poolCtx, cancel := context.WithCancel(ctx)
	return &DataCarrierPool{
		ctx:             poolCtx,
		cancel:          cancel,
		dial:            dialer,
		config:          config,
		queue:           make(chan struct{}, config.QueueDepth),
		accepted:        make(chan dataCarrierAcceptResult, config.QueueDepth),
		done:            make(chan struct{}),
		ready:           make(chan struct{}),
		state:           DataCarrierPoolDisconnected,
		refreshedAt:     time.Now(),
		targets:         append([]DataCarrierTarget(nil), config.Targets...),
		targetExpiry:    config.TargetsExpireAt,
		maximumCarriers: config.MaximumCarriers,
		retries:         make(map[int]carrierSlotRetry),
	}, nil
}

func (p *DataCarrierPool) Connect(ctx context.Context) error {
	if p == nil || ctx == nil {
		return ErrInvalidDataCarrierConfig
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.connectMu.Lock()
	defer p.connectMu.Unlock()
	p.mu.Lock()
	if p.state == DataCarrierPoolClosed {
		p.mu.Unlock()
		return ErrDataCarrierClosed
	}
	if p.state == DataCarrierPoolDraining {
		p.mu.Unlock()
		return ErrDataCarrierDraining
	}
	if len(p.carriers) == 0 {
		p.state = DataCarrierPoolConnecting
	}
	p.mu.Unlock()

	var errs []error
	if p.config.RefreshTargets != nil && time.Since(p.refreshedAt) >= 4*time.Second {
		refreshCtx, stopRefresh := context.WithTimeout(ctx, 5*time.Second)
		stopPool := context.AfterFunc(p.ctx, stopRefresh)
		targets, expiry, err := p.config.RefreshTargets(refreshCtx)
		stopPool()
		stopRefresh()
		p.refreshedAt = time.Now()
		if err != nil {
			errs = append(errs, err)
		} else {
			next := p.config
			next.Targets, next.MaximumCarriers, next.TargetsExpireAt = targets, len(targets), expiry
			if err := next.Validate(); err != nil || !expiry.After(time.Now()) {
				errs = append(errs, ErrInvalidDataCarrierConfig)
			} else {
				p.mu.Lock()
				if !slices.Equal(p.targets, targets) {
					clear(p.retries)
				}
				p.targets = append([]DataCarrierTarget(nil), targets...)
				p.targetExpiry = expiry
				p.maximumCarriers = len(targets)
				kept := p.carriers[:0]
				var removed []*DataCarrier
				for _, carrier := range p.carriers {
					found := false
					for slot, target := range targets {
						if carrier.target == target {
							carrier.slot = slot
							found = true
							break
						}
					}
					if found {
						kept = append(kept, carrier)
					} else {
						removed = append(removed, carrier.carrier)
					}
				}
				p.carriers = kept
				p.mu.Unlock()
				for _, carrier := range removed {
					_ = carrier.closeFor(CloseTargetRemoved)
				}
			}
		}
	}
	p.removeClosed()
	for slot := 0; slot < p.maximumCarriers; slot++ {
		if !p.targetExpiry.IsZero() && !p.targetExpiry.After(time.Now()) {
			break
		}
		p.mu.RLock()
		present := false
		for _, carrier := range p.carriers {
			if carrier.slot == slot {
				present = true
			}
		}
		p.mu.RUnlock()
		if present {
			continue
		}
		if retry := p.retries[slot]; len(p.targets) > 0 && time.Now().Before(retry.next) {
			continue
		}
		deadline := time.Now().Add(5 * time.Second)
		if !p.targetExpiry.IsZero() && p.targetExpiry.Before(deadline) {
			deadline = p.targetExpiry
		}
		attemptCtx, stopAttempt := context.WithDeadline(ctx, deadline)
		result, transport, attempt, err := p.dialSlot(attemptCtx, slot)
		if err != nil {
			stopAttempt()
			errs = append(errs, err)
			p.retrySlot(slot)
			if len(p.targets) == 0 {
				break
			}
			continue
		}
		var carrier *DataCarrier
		if result.Session != nil {
			carrier, err = NewDataCarrierClientWithSession(p.ctx, result.Session, p.config.Carrier, p.config.Session)
		} else {
			carrier, err = NewDataCarrierClient(p.ctx, result.Link, p.config.Carrier, p.config.Session)
		}
		if err != nil {
			stopAttempt()
			if result.Session != nil {
				_ = result.Session.Close()
			} else if result.Link != nil {
				_ = result.Link.Close()
			}
			errs = append(errs, err)
			p.retrySlot(slot)
			if len(p.targets) == 0 {
				break
			}
			continue
		}
		pingCtx, cancel := context.WithTimeout(attemptCtx, p.config.Carrier.ConnectionWriteLimit)
		err = carrier.Ping(pingCtx)
		cancel()
		stopAttempt()
		if err != nil {
			_ = carrier.closeFor(ClosePoolDialFailed)
			errs = append(errs, err)
			p.retrySlot(slot)
			if len(p.targets) == 0 {
				break
			}
			continue
		}
		p.mu.Lock()
		if p.state == DataCarrierPoolClosed {
			p.mu.Unlock()
			_ = carrier.closeFor(ClosePoolShutdown)
			return ErrDataCarrierClosed
		}
		delete(p.retries, slot)
		p.carriers = append(p.carriers, &pooledDataCarrier{target: p.target(slot), transport: transport, edgeID: result.EdgeID, failureDomain: result.FailureDomain, slot: slot, attempt: attempt, carrier: carrier})
		if p.selected == "" {
			p.selected = transport
		}
		p.workers.Add(1)
		go p.acceptCarrier(carrier, p.target(slot))
		if transport == HTTP3 || transport == HTTP2 {
			p.workers.Add(1)
			go p.monitorCarrier(carrier, 5*time.Second, 2*time.Second)
		}
		p.mu.Unlock()
	}
	p.mu.Lock()
	if len(p.carriers) > 0 {
		p.state = DataCarrierPoolReady
		p.readyOnce.Do(func() { close(p.ready) })
	} else if p.state != DataCarrierPoolClosed {
		p.state = DataCarrierPoolDisconnected
	}
	state := p.state
	p.mu.Unlock()
	if state == DataCarrierPoolReady {
		p.repairOnce.Do(func() { p.workers.Add(1); go p.repair() })
		return nil
	}
	if len(errs) == 0 {
		return ErrDataCarrierUnavailable
	}
	return errors.Join(errs...)
}

func (p *DataCarrierPool) target(slot int) DataCarrierTarget {
	if len(p.targets) > 0 {
		return p.targets[slot]
	}
	return DataCarrierTarget{EdgeID: p.config.EdgeID, ProcessEpoch: p.config.EdgeProcessEpoch, FailureDomain: p.config.FailureDomains[slot]}
}

func (p *DataCarrierPool) dialSlot(ctx context.Context, slot int) (DataCarrierDialResult, Transport, int, error) {
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	stop := context.AfterFunc(p.ctx, cancel)
	defer cancel()
	defer stop()
	return p.dialWithFallback(bounded, slot)
}

func (p *DataCarrierPool) acceptCarrier(carrier *DataCarrier, target DataCarrierTarget) {
	defer p.workers.Done()
	for {
		stream, open, err := carrier.AcceptStream(p.ctx)
		if err != nil {
			return
		}
		stream.edgeTarget = target
		select {
		case p.accepted <- dataCarrierAcceptResult{stream: stream, open: open}:
		case <-p.ctx.Done():
			_ = stream.Close()
			return
		}
	}
}

// HTTP carriers use yamux Ping request/ack frames over their authenticated
// connection. Each probe is below 1 KiB and never reaches an application origin.
func (p *DataCarrierPool) monitorCarrier(carrier *DataCarrier, interval, timeout time.Duration) {
	defer p.workers.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastResponse := time.Now()
	misses := 0
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-carrier.Done():
			return
		case <-ticker.C:
		}
		deadline := time.Now().Add(timeout)
		staleAt := lastResponse.Add(3 * interval)
		if staleAt.Before(deadline) {
			deadline = staleAt
		}
		probe, cancel := context.WithDeadline(p.ctx, deadline)
		err := carrier.Ping(probe)
		cancel()
		if p.ctx.Err() != nil {
			return
		}
		if err == nil {
			misses = 0
			lastResponse = time.Now()
			continue
		}
		misses++
		if misses >= 3 || !time.Now().Before(staleAt) {
			_ = carrier.closeFor(CloseHealthPingMissed)
			p.removeClosed()
			return
		}
	}
}

func (p *DataCarrierPool) retrySlot(slot int) {
	retry := p.retries[slot]
	retry.failures++
	limit := min(time.Second<<min(retry.failures-1, 5), 30*time.Second)
	retry.next = time.Now().Add(time.Duration(rand.Int64N(int64(limit))) + time.Millisecond)
	p.retries[slot] = retry
}

func (p *DataCarrierPool) repair() {
	defer p.workers.Done()
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-timer.C:
		}
		p.mu.RLock()
		state := p.state
		p.mu.RUnlock()
		if state == DataCarrierPoolDraining || state == DataCarrierPoolClosed {
			return
		}
		_ = p.Connect(p.ctx)
	}
}

// preferFallbackFor is how long a pool whose preferred transport failed (UDP
// blocked) dials the fallback first, so reconnecting costs no failed QUIC
// attempt; the first connection after it tries the preferred transport again.
const preferFallbackFor = 5 * time.Minute

func (p *DataCarrierPool) dialWithFallback(ctx context.Context, slot int) (DataCarrierDialResult, Transport, int, error) {
	first, second := p.config.Preferred, p.config.Fallback
	if time.Now().Before(p.fallbackUntil) {
		first, second = second, first
	}
	request := func(transport Transport, attempt int) DataCarrierDialRequest {
		target := p.target(slot)
		return DataCarrierDialRequest{Transport: transport, Slot: slot, Attempt: attempt, EdgeID: target.EdgeID, ProcessEpoch: target.ProcessEpoch, FailureDomain: target.FailureDomain, Identity: p.config.Session}
	}
	firstRequest := request(first, 1)
	result, err := p.dial(ctx, firstRequest)
	if err == nil {
		if err := validateDataCarrierDialResult(firstRequest, result); err != nil {
			return DataCarrierDialResult{}, first, 1, err
		}
		return result, first, 1, nil
	}
	if second == first || !transportFallbackAllowed(err) {
		return DataCarrierDialResult{}, first, 1, &TransportDialError{Transport: first, Err: err, Fallback: false}
	}
	secondRequest := request(second, 2)
	secondResult, secondErr := p.dial(ctx, secondRequest)
	if secondErr == nil {
		if err := validateDataCarrierDialResult(secondRequest, secondResult); err != nil {
			return DataCarrierDialResult{}, second, 2, err
		}
		if second == p.config.Fallback {
			p.fallbackUntil = time.Now().Add(preferFallbackFor)
		} else {
			p.fallbackUntil = time.Time{}
		}
		return secondResult, second, 2, nil
	}
	return DataCarrierDialResult{}, first, 1, errors.Join(
		&TransportDialError{Transport: first, Err: err, Fallback: true},
		&TransportDialError{Transport: second, Err: secondErr, Fallback: false},
	)
}

func validateDataCarrierDialResult(request DataCarrierDialRequest, result DataCarrierDialResult) error {
	if (result.Link == nil) == (result.Session == nil) || result.Transport != request.Transport || result.EdgeID != request.EdgeID || result.FailureDomain != request.FailureDomain || result.PeerIdentity != request.Identity {
		if result.Link != nil {
			_ = result.Link.Close()
		}
		if result.Session != nil {
			_ = result.Session.Close()
		}
		return fmt.Errorf("%w: dial result identity does not match slot %d attempt %d", ErrInvalidDataCarrierConfig, request.Slot, request.Attempt)
	}
	return nil
}

func (p *DataCarrierPool) Ready() <-chan struct{} {
	if p == nil {
		return closedDataCarrierChannel()
	}
	return p.ready
}

func (p *DataCarrierPool) State() DataCarrierPoolState {
	if p == nil {
		return DataCarrierPoolClosed
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state
}

func (p *DataCarrierPool) SelectedTransport() (Transport, bool) {
	if p == nil {
		return "", false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.selected, p.selected != ""
}

func (p *DataCarrierPool) Snapshot() []DataCarrierInfo {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]DataCarrierInfo, 0, len(p.carriers))
	for _, pooled := range p.carriers {
		if pooled == nil || pooled.carrier == nil {
			continue
		}
		result = append(result, DataCarrierInfo{ProcessEpoch: pooled.target.ProcessEpoch, ID: pooled.carrier.ID(), Identity: pooled.carrier.Identity(), Transport: pooled.transport, EdgeID: pooled.edgeID, FailureDomain: pooled.failureDomain, Slot: pooled.slot, Attempt: pooled.attempt, ActiveStreams: pooled.carrier.ActiveStreams(), State: pooled.carrier.State()})
	}
	return result
}

func (p *DataCarrierPool) OpenStream(ctx context.Context, open ...StreamOpen) (io.ReadWriteCloser, error) {
	if len(open) > 1 {
		return nil, ErrInvalidDataCarrierConfig
	}
	var metadata *StreamOpen
	if len(open) == 1 {
		metadata = &open[0]
	}
	return p.openStream(ctx, false, metadata)
}

func (p *DataCarrierPool) OpenControlStream(ctx context.Context) (io.ReadWriteCloser, error) {
	if p == nil || ctx == nil {
		return nil, ErrInvalidDataCarrierConfig
	}
	p.mu.Lock()
	if p.control != nil || p.controlOpening {
		p.mu.Unlock()
		return nil, ErrDataCarrierControlOpen
	}
	p.controlOpening = true
	p.mu.Unlock()
	stream, err := p.openStream(ctx, true, nil)
	if err != nil {
		p.mu.Lock()
		p.controlOpening = false
		p.mu.Unlock()
		return nil, err
	}
	p.mu.Lock()
	if p.control != nil {
		p.controlOpening = false
		p.mu.Unlock()
		_ = stream.Close()
		return nil, ErrDataCarrierControlOpen
	}
	control, ok := stream.(*DataCarrierStream)
	if !ok {
		p.mu.Unlock()
		_ = stream.Close()
		return nil, ErrDataCarrierUnavailable
	}
	p.control = control
	p.controlOpening = false
	p.mu.Unlock()
	return control, nil
}

func (p *DataCarrierPool) openStream(ctx context.Context, control bool, open *StreamOpen) (io.ReadWriteCloser, error) {
	if p == nil || ctx == nil {
		return nil, ErrInvalidDataCarrierConfig
	}
	p.mu.RLock()
	state := p.state
	p.mu.RUnlock()
	if state == DataCarrierPoolDraining {
		return nil, ErrDataCarrierDraining
	}
	if state == DataCarrierPoolClosed {
		return nil, ErrDataCarrierClosed
	}
	select {
	case p.queue <- struct{}{}:
		defer func() { <-p.queue }()
	case <-p.done:
		return nil, ErrDataCarrierClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := p.ensureConnected(ctx); err != nil {
		return nil, err
	}
	for {
		p.removeClosed()
		p.mu.RLock()
		carriers := append([]*pooledDataCarrier(nil), p.carriers...)
		p.mu.RUnlock()
		if len(carriers) == 0 {
			return nil, ErrDataCarrierUnavailable
		}
		if control {
			// Keep the control stream on the selected carrier.  This gives
			// control ownership a stable diagnostic and leaves other carriers
			// available for data streams.
			selected, ok := p.SelectedTransport()
			if !ok {
				return nil, ErrDataCarrierUnavailable
			}
			for _, pooled := range carriers {
				if pooled.transport == selected {
					return pooled.carrier.openStreamWithMetadata(ctx, p.clearControl, nil)
				}
			}
			return nil, ErrDataCarrierUnavailable
		}
		// Pick the least-loaded carrier.  Sorting is unnecessary because the
		// list is small; preserving list order makes selection deterministic.
		var selected *pooledDataCarrier
		for _, pooled := range carriers {
			if pooled.carrier.State() == DataCarrierClosed {
				continue
			}
			if selected == nil || pooled.carrier.ActiveStreams() < selected.carrier.ActiveStreams() {
				selected = pooled
			}
		}
		if selected == nil {
			if err := p.ensureConnected(ctx); err != nil {
				return nil, err
			}
			continue
		}
		stream, err := selected.carrier.openStreamWithMetadata(ctx, nil, open)
		if err == nil {
			return stream, nil
		}
		if errors.Is(err, ErrDataCarrierClosed) {
			p.removeClosed()
			continue
		}
		return nil, err
	}
}

// AcceptStream waits for an edge-opened data stream on any healthy carrier.
// Pool-owned waiters preserve every accepted stream in a bounded queue.
// Caller cancellation never consumes another carrier's stream.
func (p *DataCarrierPool) AcceptStream(ctx context.Context) (*DataCarrierStream, StreamOpen, error) {
	if p == nil || ctx == nil {
		return nil, StreamOpen{}, ErrInvalidDataCarrierConfig
	}
	p.mu.RLock()
	state := p.state
	p.mu.RUnlock()
	if state == DataCarrierPoolDraining {
		return nil, StreamOpen{}, ErrDataCarrierDraining
	}
	if state == DataCarrierPoolClosed {
		return nil, StreamOpen{}, ErrDataCarrierClosed
	}
	select {
	case <-p.ready: // After first readiness the repair owner recovers missing carriers.
	default:
		if err := p.ensureConnected(ctx); err != nil {
			return nil, StreamOpen{}, err
		}
	}
	select {
	case result := <-p.accepted:
		return result.stream, result.open, result.err
	case <-ctx.Done():
		return nil, StreamOpen{}, ctx.Err()
	case <-p.done:
		return nil, StreamOpen{}, ErrDataCarrierClosed
	}
}

func (p *DataCarrierPool) clearControl() {
	p.mu.Lock()
	p.control = nil
	p.mu.Unlock()
}

func (p *DataCarrierPool) ensureConnected(ctx context.Context) error {
	p.removeClosed()
	p.mu.RLock()
	count := len(p.carriers)
	state := p.state
	p.mu.RUnlock()
	if count > 0 && state == DataCarrierPoolReady {
		return nil
	}
	return p.Connect(ctx)
}

func (p *DataCarrierPool) removeClosed() {
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := p.carriers[:0]
	for _, pooled := range p.carriers {
		if pooled != nil && pooled.carrier != nil && pooled.carrier.State() != DataCarrierClosed {
			kept = append(kept, pooled)
		}
	}
	p.carriers = kept
	if len(kept) == 0 {
		if p.state == DataCarrierPoolReady {
			p.state = DataCarrierPoolDisconnected
		}
		p.selected = ""
		return
	}
	selectedPresent := false
	for _, pooled := range kept {
		if pooled.transport == p.selected {
			selectedPresent = true
			break
		}
	}
	if !selectedPresent {
		p.selected = kept[0].transport
	}
}

// BeginDrain atomically closes stream admission without waiting for existing
// streams or closing their underlying carriers. It is idempotent and fences a
// concurrent Connect so a stale dial completion cannot reopen admission.
func (p *DataCarrierPool) BeginDrain() error {
	if p == nil {
		return ErrInvalidDataCarrierConfig
	}
	p.connectMu.Lock()
	defer p.connectMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == DataCarrierPoolClosed || p.state == DataCarrierPoolDraining {
		return nil
	}
	p.state = DataCarrierPoolDraining
	return nil
}

// ActiveStreams returns the bounded aggregate stream count across the exact
// authenticated carrier generation owned by this pool.
func (p *DataCarrierPool) ActiveStreams() int { return p.totalActive() }

func (p *DataCarrierPool) Drain(ctx context.Context) error {
	if p == nil || ctx == nil {
		return ErrInvalidDataCarrierConfig
	}
	if err := p.BeginDrain(); err != nil {
		return err
	}
	for {
		p.removeClosed()
		if p.totalActive() == 0 {
			return p.Close()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.done:
			return nil
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (p *DataCarrierPool) totalActive() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	total := 0
	for _, pooled := range p.carriers {
		if pooled != nil && pooled.carrier != nil {
			total += pooled.carrier.ActiveStreams()
		}
	}
	return total
}

func (p *DataCarrierPool) Close() error {
	if p == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		p.cancel()
		p.connectMu.Lock()
		p.mu.Lock()
		p.state = DataCarrierPoolClosed
		carriers := append([]*pooledDataCarrier(nil), p.carriers...)
		p.mu.Unlock()
		p.connectMu.Unlock()
		close(p.done)
		p.cancel()
		p.readyOnce.Do(func() { close(p.ready) })
		for _, pooled := range carriers {
			if pooled != nil && pooled.carrier != nil {
				_ = pooled.carrier.closeFor(ClosePoolShutdown)
			}
		}
		p.workers.Wait()
		for {
			select {
			case result := <-p.accepted:
				if result.stream != nil {
					_ = result.stream.Close()
				}
			default:
				return
			}
		}
	})
	return nil
}
