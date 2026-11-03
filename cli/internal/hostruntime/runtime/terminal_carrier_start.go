//go:build darwin || linux || windows

package runtime

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
)

func (c *terminalCarrier) start(ctx context.Context, base *carrierIdentity) error {
	identity, err := c.owner.PrepareCarrierIdentity()
	if err != nil {
		return err
	}
	_, source, err := installsource.Current()
	if err != nil {
		return err
	}
	// The RPC context bounds staging, not the new connection's lifetime.
	// The independent owner explicitly closes this transport when drained.
	run, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.cancel = cancel
	c.run = run
	c.runDone = run.Done()
	stopStaging := context.AfterFunc(ctx, cancel)
	defer stopStaging()
	if err := c.authorization.Start(run); err != nil {
		return err
	}
	events := base.events.Subscribe(carrierRenewalTopics...)
	c.network, err = c.owner.StartCarrierNetwork(run, identity, source.SHA256[:32], c.apps.Serve)
	if err != nil {
		return err
	}
	c.offer = identity.Offer
	c.workers.Add(2)
	go func() {
		defer c.workers.Done()
		_ = c.authority.Run(run, c.control, nil)
		cancel()
	}()
	go func() {
		defer c.workers.Done()
		for {
			select {
			case <-run.Done():
				return
			case <-events.Changed():
				c.authority.RequestUrgentRefresh()
			}
		}
	}()
	return nil
}

func (c *terminalCarrier) ready() bool {
	if c == nil || c.runDone == nil || c.offer.Claims.ExpiresUnix <= time.Now().Unix() || c.network == nil || c.network.Sealed() {
		return false
	}
	if _, err := c.authority.Self(); err != nil {
		return false
	}
	select {
	case <-c.runDone:
		return false
	default:
		return true
	}
}
