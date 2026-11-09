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
	root := t.TempDir()
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
		recover: func(ctx context.Context) error {
			k.recoveries++
			if err := manager.Recover(ctx); err != nil {
				return err
			}
			k.candidateOwner = false
			return os.Remove(record)
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
	k.dead.recover = func(context.Context) error { return failed }
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
		recover: func(context.Context) error { recovered.Add(1); pending.Store(false); return nil },
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
