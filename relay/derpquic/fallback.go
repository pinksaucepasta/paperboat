package derpquic

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/netip"
	"sync"
	"time"

	"tailscale.com/derp"
	"tailscale.com/types/key"
)

// Carrier is the magicsock carrier boundary. FallbackCarrier keeps WSS as a
// reachability fallback without enabling native DERP/TCP.
type Carrier interface {
	Connect(context.Context) error
	Close() error
	Send(key.NodePublic, []byte) error
	SendControl(key.NodePublic, []byte) error
	RecvDetail() (derp.ReceivedMessage, int, error)
	NotePreferred(bool)
	SendPong([8]byte) error
	LocalAddr() (netip.AddrPort, error)
	Ping(context.Context) error
}

type FallbackCarrier struct {
	preferred   Carrier
	fallback    Carrier
	recovery    time.Duration
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.RWMutex
	connectGate chan struct{}
	active      Carrier
	fatal       error
	closed      bool
	workers     sync.WaitGroup
	recovering  bool
}

const preferredAttemptTimeout = 2 * time.Second

func NewFallbackCarrier(preferred, fallback Carrier, recovery time.Duration) *FallbackCarrier {
	if recovery <= 0 {
		recovery = 5 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &FallbackCarrier{preferred: preferred, fallback: fallback, recovery: recovery, ctx: ctx, cancel: cancel, connectGate: make(chan struct{}, 1)}
}

func fatalCarrier(err error) bool {
	var f interface{ Fatal() bool }
	return errors.As(err, &f) && f.Fatal()
}

func (c *FallbackCarrier) Connect(ctx context.Context) error {
	if err := acquireConnect(ctx, c.ctx, c.connectGate); err != nil {
		return err
	}
	defer releaseConnect(c.connectGate)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	if c.fatal != nil {
		err := c.fatal
		c.mu.Unlock()
		return err
	}
	if c.active != nil {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()
	preferredCtx, cancelPreferred := context.WithTimeout(ctx, preferredAttemptTimeout)
	stopCancel := context.AfterFunc(c.ctx, cancelPreferred)
	err := c.preferred.Connect(preferredCtx)
	stopCancel()
	cancelPreferred()
	if err == nil {
		c.mu.Lock()
		c.active = c.preferred
		c.mu.Unlock()
		return nil
	} else if fatalCarrier(err) {
		c.latch(err)
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.fallback.Connect(ctx); err != nil {
		if fatalCarrier(err) {
			c.latch(err)
		}
		return err
	}
	c.mu.Lock()
	c.active = c.fallback
	c.startRecoverLocked()
	c.mu.Unlock()
	return nil
}

func (c *FallbackCarrier) startRecoverLocked() {
	if c.recovering || c.ctx.Err() != nil {
		return
	}
	c.recovering = true
	c.workers.Add(1)
	go c.recoverPreferred()
}

func (c *FallbackCarrier) latch(err error) {
	c.mu.Lock()
	if c.fatal == nil {
		c.fatal = err
	}
	c.mu.Unlock()
}
func (c *FallbackCarrier) current() (Carrier, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.fatal != nil {
		return nil, c.fatal
	}
	if c.active == nil {
		return nil, ErrClosed
	}
	return c.active, nil
}

func (c *FallbackCarrier) recoverPreferred() {
	defer func() { c.mu.Lock(); c.recovering = false; c.mu.Unlock(); c.workers.Done() }()
	failures := uint(0)
	timer := time.NewTimer(recoveryRetryDelay(c.recovery, failures, rand.Uint64()))
	defer timer.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-timer.C:
			ctx, cancel := context.WithTimeout(c.ctx, preferredAttemptTimeout)
			if err := acquireConnect(ctx, c.ctx, c.connectGate); err != nil {
				cancel()
				if c.ctx.Err() != nil {
					return
				}
				failures++
				timer.Reset(recoveryRetryDelay(c.recovery, failures, rand.Uint64()))
				continue
			}
			c.mu.RLock()
			active := c.active
			fatal := c.fatal
			c.mu.RUnlock()
			if fatal != nil || active != c.fallback {
				releaseConnect(c.connectGate)
				cancel()
				return
			}
			err := c.preferred.Connect(ctx)
			if err == nil {
				err = c.preferred.Ping(ctx)
			}
			cancel()
			if fatalCarrier(err) {
				c.latch(err)
				_ = disconnectCarrier(c.fallback)
				releaseConnect(c.connectGate)
				return
			}
			if err != nil {
				releaseConnect(c.connectGate)
				failures++
				timer.Reset(recoveryRetryDelay(c.recovery, failures, rand.Uint64()))
				continue
			}
			c.mu.Lock()
			if c.active == c.fallback && c.fatal == nil {
				c.active = c.preferred
				c.mu.Unlock()
				_ = disconnectCarrier(c.fallback)
				releaseConnect(c.connectGate)
				return
			}
			c.mu.Unlock()
			releaseConnect(c.connectGate)
		}
	}
}

func recoveryRetryDelay(base time.Duration, failures uint, random uint64) time.Duration {
	maximum := 2 * base
	nominal := base
	if failures > 0 {
		nominal = maximum
	}
	// Select 80%..120% in one-per-mille steps, then retain the hard cap.
	delay := nominal * time.Duration(800+random%401) / 1000
	if delay > maximum {
		return maximum
	}
	return delay
}

func disconnectCarrier(carrier Carrier) error {
	if disconnect, ok := carrier.(interface{ Disconnect() error }); ok {
		return disconnect.Disconnect()
	}
	return carrier.Close()
}

func (c *FallbackCarrier) Close() error {
	c.cancel()
	c.connectGate <- struct{}{}
	defer releaseConnect(c.connectGate)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.workers.Wait()
		return nil
	}
	c.closed = true
	c.active = nil
	c.mu.Unlock()
	_ = c.preferred.Close()
	_ = c.fallback.Close()
	c.workers.Wait()
	return nil
}

func acquireConnect(ctx, lifetime context.Context, gate chan struct{}) error {
	// Receive/send use the lifetime as their call context. Once it is closed,
	// both Done channels can be ready; select must not choose the error contract.
	if lifetime.Err() != nil {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		if lifetime.Err() != nil {
			return ErrClosed
		}
		return ctx.Err()
	case <-lifetime.Done():
		return ErrClosed
	}
}

func releaseConnect(gate chan struct{}) { <-gate }
func (c *FallbackCarrier) withRetry(run func(Carrier) error) error {
	a, e := c.current()
	if e != nil {
		return e
	}
	e = run(a)
	if e == nil {
		return nil
	}
	if fatalCarrier(e) {
		c.latch(e)
		return e
	}
	if a != c.preferred {
		return e
	}
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	next, e := c.switchToFallback(ctx, a)
	if e != nil {
		return e
	}
	return run(next)
}

func (c *FallbackCarrier) switchToFallback(ctx context.Context, expected Carrier) (Carrier, error) {
	if err := acquireConnect(ctx, c.ctx, c.connectGate); err != nil {
		return nil, err
	}
	defer releaseConnect(c.connectGate)
	current, err := c.current()
	if err != nil {
		return nil, err
	}
	if current != expected {
		return current, nil
	}
	if err = c.fallback.Connect(ctx); err != nil {
		if fatalCarrier(err) {
			c.latch(err)
		}
		return nil, err
	}
	c.mu.Lock()
	if c.active == expected && c.fatal == nil {
		c.active = c.fallback
		c.startRecoverLocked()
	}
	c.mu.Unlock()
	_ = disconnectCarrier(expected)
	return c.fallback, nil
}
func (c *FallbackCarrier) Send(k key.NodePublic, b []byte) error {
	return c.withRetry(func(a Carrier) error { return a.Send(k, b) })
}
func (c *FallbackCarrier) SendControl(k key.NodePublic, b []byte) error {
	return c.withRetry(func(a Carrier) error { return a.SendControl(k, b) })
}
func (c *FallbackCarrier) RecvDetail() (derp.ReceivedMessage, int, error) {
	for {
		a, e := c.current()
		if e != nil {
			return nil, 0, e
		}
		m, g, e := a.RecvDetail()
		if e == nil {
			return m, g, e
		}
		c.mu.Lock()
		if c.active != a {
			c.mu.Unlock()
			continue
		}
		if fatalCarrier(e) {
			if c.fatal == nil {
				c.fatal = e
			}
			c.mu.Unlock()
			return nil, g, e
		}
		c.mu.Unlock()
		if a == c.fallback {
			return nil, g, e
		}
		ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
		_, switchErr := c.switchToFallback(ctx, a)
		cancel()
		if switchErr != nil {
			if fatalCarrier(switchErr) {
				c.latch(switchErr)
				return nil, g, switchErr
			}
			return nil, g, e
		}
	}
}
func (c *FallbackCarrier) NotePreferred(v bool) {
	c.preferred.NotePreferred(v)
	c.fallback.NotePreferred(v)
}
func (c *FallbackCarrier) SendPong(v [8]byte) error {
	return c.withRetry(func(a Carrier) error { return a.SendPong(v) })
}
func (c *FallbackCarrier) LocalAddr() (netip.AddrPort, error) {
	a, e := c.current()
	if e != nil {
		return netip.AddrPort{}, e
	}
	return a.LocalAddr()
}
func (c *FallbackCarrier) Ping(ctx context.Context) error {
	a, err := c.current()
	if err != nil {
		return err
	}
	if a != c.preferred {
		return a.Ping(ctx)
	}
	preferredCtx, cancelPreferred := context.WithTimeout(ctx, preferredAttemptTimeout)
	stopCancel := context.AfterFunc(c.ctx, cancelPreferred)
	err = a.Ping(preferredCtx)
	stopCancel()
	cancelPreferred()
	if err == nil || fatalCarrier(err) || ctx.Err() != nil {
		if fatalCarrier(err) {
			c.latch(err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	next, err := c.switchToFallback(ctx, a)
	if err != nil {
		return err
	}
	return next.Ping(ctx)
}
