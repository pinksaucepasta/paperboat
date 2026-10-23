package localdaemon

import (
	"context"
	"sync"
)

// managedSSHOwner starts managed SSH from the inventory refresh instead of before the daemon serves its
// local API. Its start talks to the server (key registration, client configuration, alias targets);
// run first, it kept the local API socket absent for 40 s and more on a slow server, so update
// validation and pairing saw no daemon and rolled back (lanep-up, lanel-e, 2026-10-23). A start that
// fails is retried on the next refresh instead of leaving managed SSH off until a restart. The outcome
// is reported when it changes.
type managedSSHOwner struct {
	config    ManagedSSHConfig
	start     func(context.Context, ManagedSSHConfig) (*ManagedSSHRuntime, error)
	report    func(context.Context, error)
	onStarted func()

	mu       sync.Mutex
	runtime  *ManagedSSHRuntime
	reported bool
	lastCode string
}

// refresh starts managed SSH when it is not running, else refreshes its configuration. A failed start
// is reported through report, not returned.
func (o *managedSSHOwner) refresh(ctx context.Context) error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.runtime != nil {
		return o.runtime.Refresh(ctx)
	}
	runtime, err := o.start(ctx, o.config)
	code := ""
	if err != nil {
		code = ManagedSSHHealthCode(err)
	}
	if !o.reported || code != o.lastCode {
		o.report(ctx, err)
		o.reported, o.lastCode = true, code
	}
	if err != nil {
		return nil
	}
	o.runtime = runtime
	if o.onStarted != nil {
		o.onStarted()
	}
	return nil
}

func (o *managedSSHOwner) close() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.runtime == nil {
		return nil
	}
	err := o.runtime.Close()
	o.runtime = nil
	return err
}
