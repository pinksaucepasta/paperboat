package localdaemon

import (
	"context"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// managedSSHInstallTimeout bounds the first managed SSH configuration install, which asks the server.
const managedSSHInstallTimeout = 15 * time.Second

// managedSSHOwner starts managed SSH from the inventory refresh instead of before the daemon serves its
// local API. Its start talks to the server (key registration, client configuration, alias targets);
// run first, it kept the local API socket absent for 40 s and more on a slow server, so update
// validation and pairing saw no daemon and rolled back (lanep-up, lanel-e, 2026-10-23). A start that
// fails is retried on the next refresh instead of leaving managed SSH off until a restart. The outcome
// is reported when it changes. The runtime is started on lifetime, the daemon's context: its agent
// serves on that context, and starting it on a refresh's bounded context stopped the agent as soon as
// that refresh ended (round 7, dadape: no agent socket, `pb ssh` denied).
type managedSSHOwner struct {
	lifetime  context.Context
	config    ManagedSSHConfig
	start     func(context.Context, ManagedSSHConfig, []api.UserMachine) (*ManagedSSHRuntime, error)
	report    func(context.Context, error)
	onStarted func()

	mu       sync.Mutex
	runtime  *ManagedSSHRuntime
	reported bool
	lastCode string
}

// refresh starts managed SSH when it is not running, else rewrites its aliases for machines (the
// inventory's latest list). A failed start is reported through report, not returned.
// domain is the running managed SSH's alias domain, or "" before it starts.
func (o *managedSSHOwner) domain() string {
	if o == nil {
		return ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.runtime.Domain()
}

func (o *managedSSHOwner) refresh(ctx context.Context, machines []api.UserMachine) error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.runtime != nil {
		return o.runtime.Refresh(ctx, machines)
	}
	runtime, err := o.start(o.lifetime, o.config, machines)
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

// publishSSHDomain puts the managed SSH alias domain in the local snapshot, so the CLI's SSH helpers ask no server.
func publishSSHDomain(store *localapi.SnapshotStore, domain string) {
	if domain == "" {
		return
	}
	_, _ = store.Update(time.Now(), func(current *localapi.Snapshot) (localapi.Snapshot, error) {
		if current == nil {
			return localapi.Snapshot{}, localapi.ErrSnapshotUnavailable
		}
		next := *current
		next.SSHDomain = domain
		return next, nil
	})
}
