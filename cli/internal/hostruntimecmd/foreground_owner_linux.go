//go:build linux

package hostruntimecmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/sessionowner"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/updated"
)

// foregroundHost is the root-owned directory of the machine-level session-owner state a service install keeps
// under its install layout: the descriptor catalog and the pinned executable the owner runs.
const foregroundHost = foregroundState + "/host"

// foregroundOwnerStopLimit is the whole budget for stopping the session owners; hostd has drained already.
const foregroundOwnerStopLimit = 10 * time.Second

// foregroundOwners runs the session owner of the enrolled account as a child of the supervisor. A service install
// has the service manager start it on hostd's request through the updater's control socket; here the supervisor
// serves the same request with the same code and starts the child itself.
type foregroundOwners struct {
	mu      sync.Mutex
	stopped bool
	owners  map[string]*foregroundOwner
}

// foregroundOwner is one generation's controller and its descriptor as last published: a republished archive
// starts on the executable of its latest descriptor, as a rewritten service declaration would.
type foregroundOwner struct {
	controller *service.ProcessController
	d          sessionowner.Descriptor
}

var errForegroundStopped = errors.New("the supervisor is shutting down")

// stop ends every owner child and refuses new ones. Hostd has already drained, so no client is attached.
func (o *foregroundOwners) stop(ctx context.Context) {
	o.mu.Lock()
	o.stopped = true
	controllers := make([]*service.ProcessController, 0, len(o.owners))
	for _, owner := range o.owners {
		controllers = append(controllers, owner.controller)
	}
	o.mu.Unlock()
	// In parallel under one deadline: a container's stop grace is short, and one wedged owner must not use up
	// the others' share. A child still alive at the deadline is killed.
	ctx, cancel := context.WithTimeout(ctx, foregroundOwnerStopLimit)
	defer cancel()
	var wait sync.WaitGroup
	for _, controller := range controllers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_ = controller.Shutdown(ctx)
		}()
	}
	wait.Wait()
}

func (o *foregroundOwners) controller(catalog sessionowner.Catalog, d sessionowner.Descriptor, account *user.User, stdout, stderr io.Writer) (service.Controller, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.stopped {
		return nil, errForegroundStopped
	}
	if existing := o.owners[d.Generation]; existing != nil {
		existing.d = d
		return existing.controller, nil
	}
	owner := &foregroundOwner{d: d}
	owner.controller = &service.ProcessController{Command: func() *exec.Cmd {
		o.mu.Lock()
		current := owner.d
		o.mu.Unlock()
		args, environment := catalog.OwnerCommand(current)
		command := exec.Command(current.Executable, args...)
		command.Env = ownerEnvironment(account, environment)
		command.Stdout, command.Stderr = stdout, stderr
		command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(current.UID), Gid: uint32(current.GID)}}
		return command
	}}
	o.owners[d.Generation] = owner
	return owner.controller, nil
}

// ownerEnvironment is the small environment a service manager would give the owner: nothing of the container's,
// in particular no enrollment credential. HOME is the writable workspace volume, like hostd's, not the image's
// read-only passwd home.
func ownerEnvironment(account *user.User, owner map[string]string) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	environment := []string{"PATH=" + path, "HOME=" + foregroundWorkspace, "USER=" + account.Username, "LOGNAME=" + account.Username}
	for key, value := range owner {
		environment = append(environment, key+"="+value)
	}
	return environment
}

// startForegroundOwners publishes the account's session-owner descriptor, starts the owner, waits until it
// answers, then serves hostd's later start requests on socket. ctx bounds the start; serveCtx ends the control
// socket and is the caller's to cancel once hostd has drained. The owners stop when stop is called.
func startForegroundOwners(ctx, serveCtx context.Context, executable string, account *user.User, runtimeRoot, shell, socket string, stdout, stderr io.Writer) (_ *foregroundOwners, catalogRoot string, err error) {
	group, err := user.LookupGroupId(account.Gid)
	if err != nil {
		return nil, "", fmt.Errorf("reading the runtime group failed: %w", err)
	}
	uid, uidErr := strconv.Atoi(account.Uid)
	gid, gidErr := strconv.Atoi(account.Gid)
	if err := errors.Join(uidErr, gidErr); err != nil {
		return nil, "", err
	}
	source, err := installsource.Inspect(executable, buildinfo.Version, buildinfo.Distribution)
	if err != nil {
		return nil, "", fmt.Errorf("reading the pb executable's identity failed: %w", err)
	}
	for _, dir := range []string{foregroundHost, filepath.Join(foregroundHost, "declarations")} {
		if err := ensureForegroundDir(dir, 0o755, 0, 0); err != nil {
			return nil, "", err
		}
	}
	request := hostinstall.Request{Platform: "linux", User: account.Username, UID: uid, Group: group.Name, GID: gid, Executable: executable, Source: source, StateRoot: runtimeRoot, WorkspaceRoot: foregroundWorkspace, Shell: shell}
	catalog, d, err := hostinstall.ForegroundSessionOwner(ctx, request, foregroundHost)
	if err != nil {
		return nil, "", fmt.Errorf("preparing the session owner failed: %w", err)
	}
	owners := &foregroundOwners{owners: map[string]*foregroundOwner{}}
	catalog.ConfigRoot = filepath.Join(foregroundHost, "declarations")
	catalog.Process = func(d sessionowner.Descriptor) (service.Controller, error) {
		return owners.controller(catalog, d, account, stdout, stderr)
	}
	if err := catalog.Publish(ctx, d); err != nil {
		return nil, "", fmt.Errorf("publishing the session owner descriptor failed: %w", err)
	}
	// No installation runs in a container, so Reconcile may retire every other generation. A container updater
	// that stages candidates would have to say so here.
	coordinator := sessionowner.Coordinator{Catalog: catalog, Active: d, Installing: func() bool { return false }}
	if err := coordinator.Ensure(ctx, d.Generation); err != nil {
		owners.stop(context.WithoutCancel(ctx))
		return nil, "", fmt.Errorf("starting the session owner failed: %w", err)
	}
	// A replaced image version leaves its descriptor and pinned executable behind. The same pass the updater runs
	// retires them (history stays readable through the running version); it is housekeeping, not a start condition.
	if err := errors.Join(coordinator.Reconcile(ctx), hostinstall.PruneForegroundSessionOwnerPins(ctx, catalog, request, foregroundHost)); err != nil {
		fmt.Fprintln(stderr, "paperboat container: clearing replaced session owner versions failed: "+err.Error())
	}
	if err := updated.ServeSessionOwnerStart(serveCtx, socket, uid, gid, func(context.Context) (sessionowner.Coordinator, error) { return coordinator, nil }); err != nil {
		owners.stop(context.WithoutCancel(ctx))
		return nil, "", fmt.Errorf("serving session owner start requests failed: %w", err)
	}
	return owners, catalog.Root, nil
}
