// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package magicsock

import (
	"context"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

type rebindTestCarrier struct {
	DERPCarrier
	pingStarted chan struct{}
	pingRelease chan struct{}
	pingDone    chan struct{}
	pingErr     error
	localCalls  atomic.Int32
	closed      atomic.Bool
}

func (c *rebindTestCarrier) LocalAddr() (netip.AddrPort, error) {
	c.localCalls.Add(1)
	return netip.AddrPort{}, errors.New("carrier owns no inspectable local address")
}

func (c *rebindTestCarrier) Ping(ctx context.Context) error {
	if c.pingDone != nil {
		defer close(c.pingDone)
	}
	if c.pingStarted != nil {
		select {
		case c.pingStarted <- struct{}{}:
		default:
		}
	}
	if c.pingRelease != nil {
		select {
		case <-c.pingRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return c.pingErr
}

func (c *rebindTestCarrier) Close() error {
	c.closed.Store(true)
	return nil
}

func TestRebindInjectedCarrierWithUnknownLocalAddressUsesPing(t *testing.T) {
	c := newConn(t.Logf)
	c.derpCarrierFactory = func(key.NodePrivate, func() *tailcfg.DERPRegion) DERPCarrier { panic("unused") }
	carrier := &rebindTestCarrier{pingStarted: make(chan struct{}, 1)}
	c.activeDerp = map[tailcfg.DERPRegionID]activeDerp{1: {c: carrier, cancel: func() {}}}

	c.maybeCloseDERPsOnRebind([]netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")})
	select {
	case <-carrier.pingStarted:
	case <-time.After(time.Second):
		t.Fatal("injected carrier was not pinged")
	}
	if carrier.localCalls.Load() != 0 {
		t.Fatalf("LocalAddr called %d times for injected carrier", carrier.localCalls.Load())
	}
	c.mu.Lock()
	_, active := c.activeDerp[1]
	c.mu.Unlock()
	if !active || carrier.closed.Load() {
		t.Fatal("healthy injected carrier was closed on rebind")
	}
}

func TestRebindStalePingFailureDoesNotCloseReplacementCarrier(t *testing.T) {
	c := newConn(t.Logf)
	c.derpCarrierFactory = func(key.NodePrivate, func() *tailcfg.DERPRegion) DERPCarrier { panic("unused") }
	release := make(chan struct{})
	done := make(chan struct{})
	old := &rebindTestCarrier{pingStarted: make(chan struct{}, 1), pingRelease: release, pingDone: done, pingErr: errors.New("stale failure")}
	replacement := &rebindTestCarrier{}
	c.activeDerp = map[tailcfg.DERPRegionID]activeDerp{1: {c: old, cancel: func() {}}}

	c.maybeCloseDERPsOnRebind(nil)
	select {
	case <-old.pingStarted:
	case <-time.After(time.Second):
		t.Fatal("old carrier was not pinged")
	}
	c.mu.Lock()
	c.activeDerp[1] = activeDerp{c: replacement, cancel: func() {}}
	c.mu.Unlock()
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("failed ping did not return")
	}
	c.mu.Lock()
	current := c.activeDerp[1].c
	c.mu.Unlock()
	if current != replacement || replacement.closed.Load() {
		t.Fatal("stale ping failure closed the replacement carrier")
	}
}
