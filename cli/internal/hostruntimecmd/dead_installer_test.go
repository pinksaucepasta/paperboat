package hostruntimecmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
)

// lifecycleRole is a native service as the lifecycle journal sees it.
type lifecycleRole struct {
	id      string
	running bool
}

func (r *lifecycleRole) ID() string { return r.id }
func (r *lifecycleRole) Inspect(context.Context) (service.NativeComponentStatus, error) {
	return service.NativeComponentStatus{ID: r.id, Installed: true, Enabled: true, Running: r.running, Ready: r.running, Definition: []byte("pb " + r.id)}, nil
}
func (r *lifecycleRole) Install(context.Context) error   { return nil }
func (r *lifecycleRole) Start(context.Context) error     { r.running = true; return nil }
func (r *lifecycleRole) Repair(context.Context) error    { return nil }
func (r *lifecycleRole) Stop(context.Context) error      { r.running = false; return nil }
func (r *lifecycleRole) Uninstall(context.Context) error { return nil }
func (r *lifecycleRole) Restore(_ context.Context, before service.NativeComponentStatus) error {
	r.running = before.Running
	return nil
}

type installedVersion struct{ path string }

func (s installedVersion) Capture(context.Context) ([]byte, error) { return os.ReadFile(s.path) }
func (s installedVersion) Restore(_ context.Context, data []byte) error {
	return os.WriteFile(s.path, data, 0o600)
}

// killedInstall is an installation whose installer died after it stopped the services (victus 2026-11-10: hostd,
// the local daemon and the updater stayed stopped and the candidate's session owner stayed running).
type killedInstall struct {
	roles          []*lifecycleRole
	version        string
	candidateOwner bool
	live           bool
	recoveries     int
	unlocks        int
	dead           deadInstaller
}

func newKilledInstall(t *testing.T) *killedInstall {
	t.Helper()
	root := shorttemp.Dir(t) // the lifecycle journal refuses a symlinked ancestor; macOS's TempDir is under /var -> /private/var
	k := &killedInstall{version: filepath.Join(root, "version"), roles: []*lifecycleRole{{"hostd", true}, {"daemon", true}, {"updater", true}}}
	record := filepath.Join(root, "install-candidate.json")
	if err := os.WriteFile(k.version, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := service.NewLifecycleManager(service.LifecycleConfig{StateRoot: filepath.Join(root, "lifecycle"), NativeInstallation: installedVersion{k.version},
		Components: []service.TransactionalComponent{k.roles[0], k.roles[1], k.roles[2]}})
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() { _ = recover() }() // the installer dies: no rollback, the journal stays
		if err := os.WriteFile(record, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = manager.Replace(context.Background(), func(ctx context.Context) error {
			k.candidateOwner = true
			for _, role := range k.roles {
				_ = role.Stop(ctx)
			}
			panic("killed")
		})
	}()
	k.dead = deadInstaller{
		pending: func() (bool, error) {
			_, err := os.Lstat(record)
			return err == nil, nil
		},
		tryLock: func(context.Context) (func() error, error) {
			if k.live {
				return nil, errInstallerLive
			}
			return func() error { k.unlocks++; return nil }, nil
		},
		recover: func(ctx context.Context) (func(context.Context) error, error) {
			k.recoveries++
			if err := manager.Recover(ctx); err != nil {
				return nil, err
			}
			k.candidateOwner = false
			return nil, os.Remove(record)
		},
	}
	return k
}

func (k *killedInstall) servicesRunning() bool {
	for _, role := range k.roles {
		if !role.running {
			return false
		}
	}
	return true
}

func TestADeadInstallersStoppedServicesAreRestoredAndItsCandidateWithdrawn(t *testing.T) {
	k := newKilledInstall(t)
	if k.servicesRunning() || !k.candidateOwner {
		t.Fatal("the killed install did not leave the services stopped and its candidate running")
	}
	acted, err := k.dead.check(context.Background())
	if err != nil || !acted {
		t.Fatalf("acted=%v err=%v", acted, err)
	}
	version, _ := os.ReadFile(k.version)
	if !k.servicesRunning() || k.candidateOwner || string(version) != "previous" || k.unlocks != 1 {
		t.Fatalf("running=%v candidate=%v version=%q unlocks=%d", k.servicesRunning(), k.candidateOwner, version, k.unlocks)
	}
	// Nothing is left: the next look does not recover again.
	if acted, err := k.dead.check(context.Background()); err != nil || acted || k.recoveries != 1 {
		t.Fatalf("second look: acted=%v err=%v recoveries=%d", acted, err, k.recoveries)
	}
}

func TestALiveInstallerIsLeftAlone(t *testing.T) {
	k := newKilledInstall(t)
	k.live = true
	acted, err := k.dead.check(context.Background())
	if err != nil || acted || k.recoveries != 0 || k.servicesRunning() || !k.candidateOwner {
		t.Fatalf("acted=%v err=%v recoveries=%d running=%v candidate=%v", acted, err, k.recoveries, k.servicesRunning(), k.candidateOwner)
	}
}

func TestAFailedRecoveryReleasesTheLocksAndIsReported(t *testing.T) {
	k := newKilledInstall(t)
	failed := errors.New("the previous version's hostd did not start")
	k.dead.recover = func(context.Context) (func(context.Context) error, error) { return nil, failed }
	if acted, err := k.dead.check(context.Background()); !errors.Is(err, failed) || acted || k.unlocks != 1 {
		t.Fatalf("acted=%v err=%v unlocks=%d", acted, err, k.unlocks)
	}
}

// The watch looks at once (a service that just started at boot does not wait a poll), keeps looking after a
// recovery, and ends with its context.
func TestTheWatchLooksAtOnceAndAgainAfterARecovery(t *testing.T) {
	var looks, recovered atomic.Int32
	pending := atomic.Bool{}
	pending.Store(true)
	dead := deadInstaller{
		pending: func() (bool, error) { looks.Add(1); return pending.Load(), nil },
		tryLock: func(context.Context) (func() error, error) { return func() error { return nil }, nil },
		recover: func(context.Context) (func(context.Context) error, error) {
			recovered.Add(1)
			pending.Store(false)
			return nil, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		dead.watch(ctx, time.Hour, time.Hour, func(error) {})
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for recovered.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if recovered.Load() != 1 {
		t.Fatal("the first look waited for a poll")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch outlived its context")
	}
}

// carrierProcess is the carrier's own service process, the one the recovery runs in: once its service is stopped
// or removed the process is gone, and no service manager call it would have made next happens.
type carrierProcess struct {
	ended   bool
	atEnd   func() // what was already done when the process ended
	removed bool
}

var errCarrierProcessEnded = errors.New("the recovering process ended with its own service")

// processRole is a service as seen from the carrier's process.
type processRole struct {
	lifecycleRole
	process   *carrierProcess
	installed bool
	self      bool // the carrier itself
}

func (r *processRole) Inspect(context.Context) (service.NativeComponentStatus, error) {
	if !r.installed {
		return service.NativeComponentStatus{ID: r.id}, nil
	}
	return service.NativeComponentStatus{ID: r.id, Installed: true, Enabled: true, Running: r.running, Ready: r.running, Definition: []byte("pb " + r.id)}, nil
}

// end is the service manager stopping the carrier: the calling process dies in that call.
func (r *processRole) end() error {
	if !r.process.ended {
		r.process.atEnd()
	}
	r.process.ended, r.running = true, false
	return errCarrierProcessEnded
}

func (r *processRole) Restore(_ context.Context, before service.NativeComponentStatus) error {
	if r.process.ended {
		return errCarrierProcessEnded
	}
	if r.self && !before.Running {
		return r.end() // Uninstall: stop first, and the caller does not live to delete
	}
	r.installed, r.running = before.Installed, before.Running
	return nil
}

// RemoveSelf is the removal from inside the service: the declaration first, the stop that ends the caller last.
func (r *processRole) RemoveSelf(context.Context) error {
	r.installed, r.process.removed = false, true
	return r.end()
}

// carrierCreatingInstall is an install that created the carrier (a first pairing: the machine had none), stopped
// the other services, and died. Its recovery runs in that carrier and has to remove it. self is what the recovery
// keeps for last.
type carrierCreatingInstall struct {
	dead           deadInstaller
	process        *carrierProcess
	carrier        *processRole
	others         []*processRole
	candidateOwner bool
	unlocked       bool
	journalGone    func() bool
	doneAtEnd      bool // everything but the carrier's own removal was done when its process ended
}

func (k *carrierCreatingInstall) othersRunning() bool {
	for _, role := range k.others {
		if !role.running {
			return false
		}
	}
	return true
}

func newCarrierCreatingInstall(t *testing.T, self string) *carrierCreatingInstall {
	t.Helper()
	root := shorttemp.Dir(t)
	k := &carrierCreatingInstall{process: &carrierProcess{}}
	role := func(id string, installed bool) *processRole {
		return &processRole{lifecycleRole: lifecycleRole{id: id, running: installed}, process: k.process, installed: installed, self: id == service.CarrierKind}
	}
	k.carrier, k.others = role(service.CarrierKind, false), []*processRole{role("hostd", true), role("daemon", true), role("updater", true)}
	version, record := filepath.Join(root, "version"), filepath.Join(root, "install-candidate.json")
	for path, body := range map[string]string{version: "previous", record: "{}"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := service.NewLifecycleManager(service.LifecycleConfig{StateRoot: filepath.Join(root, "lifecycle"), NativeInstallation: installedVersion{version},
		Components: []service.TransactionalComponent{k.carrier, k.others[0], k.others[1], k.others[2]}})
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() { _ = recover() }() // the installer dies: no rollback, the journal stays
		_ = manager.Replace(context.Background(), func(context.Context) error {
			k.candidateOwner = true
			k.carrier.installed, k.carrier.running = true, true // the dead installer's carrier, with the watch in it
			for _, other := range k.others {
				other.running = false
			}
			panic("killed")
		})
	}()
	k.journalGone = func() bool { return manager.RequireRecovered(context.Background()) == nil }
	k.process.atEnd = func() {
		k.doneAtEnd = k.othersRunning() && !k.candidateOwner && k.journalGone() && k.unlocked
	}
	k.dead = deadInstaller{
		pending: func() (bool, error) {
			_, err := os.Lstat(record)
			return err == nil, nil
		},
		tryLock: func(context.Context) (func() error, error) {
			return func() error { k.unlocked = true; return nil }, nil
		},
		recover: func(ctx context.Context) (func(context.Context) error, error) {
			finishSelf, err := manager.RecoverWithin(ctx, self)
			if err != nil {
				return nil, err
			}
			k.candidateOwner = false
			return finishSelf, os.Remove(record)
		},
	}
	return k
}

// A killed install that created the carrier (`pb pair` killed after the carrier started) is recovered by that
// carrier's own watch, which has to remove the service it runs in. Removed in rollback order (the carrier is
// first) the process died there: hostd, the daemon and the updater stayed stopped and the journal stayed. Kept
// for last, everything else is restored, the candidate withdrawn, the journal and the locks released, and only
// then the carrier removes itself.
func TestADeadInstallersCarrierRemovesItselfLastAfterEverythingElseIsRestored(t *testing.T) {
	k := newCarrierCreatingInstall(t, service.CarrierKind)
	// The removal's last call does not return to a live process; the fake reports it as this error.
	if acted, err := k.dead.check(context.Background()); !errors.Is(err, errCarrierProcessEnded) || acted {
		t.Fatalf("acted=%v err=%v", acted, err)
	}
	if !k.process.ended || !k.process.removed || k.carrier.installed {
		t.Fatalf("the carrier the dead installer created stayed: ended=%v removed=%v installed=%v", k.process.ended, k.process.removed, k.carrier.installed)
	}
	if !k.doneAtEnd {
		t.Fatalf("the carrier's process ended before the recovery was complete: services=%v candidate=%v journal gone=%v unlocked=%v",
			k.othersRunning(), k.candidateOwner, k.journalGone(), k.unlocked)
	}
	if pending, _ := k.dead.pending(); pending {
		t.Fatal("the record stayed: the next look would recover again")
	}
}

// Why the carrier is kept for last: recovered like any outside caller's (no self), its removal comes first and
// ends the process with every other service still stopped and the journal still pending.
func TestWithoutKeepingTheCarrierForLastItsRecoveryDiesAtTheCarrier(t *testing.T) {
	k := newCarrierCreatingInstall(t, "")
	if acted, err := k.dead.check(context.Background()); err == nil || acted {
		t.Fatalf("acted=%v err=%v", acted, err)
	}
	if !k.process.ended || k.othersRunning() || k.journalGone() || k.doneAtEnd {
		t.Fatalf("ended=%v services=%v journal gone=%v", k.process.ended, k.othersRunning(), k.journalGone())
	}
}
