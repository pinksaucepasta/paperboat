//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	hostruntime "github.com/pinksaucepasta/paperboat/internal/hostruntime/runtime"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

type hostdHost interface {
	StartHostd(context.Context) error
	ShutdownHostd(context.Context) error
	WorkloadStatus() hostdproto.WorkloadStatus
	UpdateGate() hostdproto.UpdateGateHandler
	OpenSSHStreams(context.Context) (int, error)
}

type hostdHostFactory func(context.Context, string, func(string) string) (hostdHost, error)
type hostdWorkerFactory func(context.Context, workerStartRequest) (workerProcess, error)

// runHostd starts hostd-owned workloads first, then launches the active
// runtime artifact as a separately fenced child. No coordination runtime is
// started in-process here.
func runHostd(ctx context.Context, args []string, output io.Writer) error {
	if len(args) != 0 {
		return errors.New("hostd does not accept arguments")
	}
	return runHostdWith(ctx, output,
		func(ctx context.Context, version string, environ func(string) string) (hostdHost, error) {
			return hostruntime.NewProductionOwner(ctx, version, environ)
		},
		func(ctx context.Context, request workerStartRequest) (workerProcess, error) {
			return (nativeWorkerStarter{}).Start(ctx, request)
		},
	)
}

func runHostdWith(ctx context.Context, output io.Writer, newHost hostdHostFactory, startWorker hostdWorkerFactory) (resultErr error) {
	if newHost == nil || startWorker == nil {
		return errors.New("hostd requires lifecycle factories")
	}
	socket, tokenPath, executable := os.Getenv("PAPERBOAT_HOSTD_SOCKET"), os.Getenv("PAPERBOAT_HOSTD_TOKEN_FILE"), os.Getenv("PAPERBOAT_RUNTIME_CURRENT")
	if !filepath.IsAbs(socket) || !filepath.IsAbs(tokenPath) || !filepath.IsAbs(executable) {
		return errors.New("hostd requires fixed socket, token, and active runtime paths")
	}
	token, err := readWorkerToken(tokenPath)
	if err != nil {
		return err
	}
	notifier, err := service.NewProcessNotifier()
	if err != nil {
		return err
	}
	if err := notifier.Starting(); err != nil {
		return err
	}
	listeners := &hostdListeners{socket: socket}
	defer func() { resultErr = errors.Join(resultErr, listeners.close()) }()
	handover, err := listeners.dialHandover(ctx, token)
	if err != nil {
		return runtimeFailure("hostd could not take over from the running generation", err, notifier.Degraded("hostd handover failed"))
	}
	if handover != nil {
		// Closing before commit leaves the old generation serving.
		defer handover.Close()
	}
	host, err := newHost(ctx, buildinfo.Version, os.Getenv)
	if err != nil {
		return runtimeFailure("hostd initialization failed", err, notifier.Degraded("hostd initialization failed"))
	}
	if err := host.StartHostd(ctx); err != nil {
		return runtimeFailure("hostd startup failed", err, notifier.Degraded("hostd startup failed"))
	}
	if err := listeners.bindRuntime(host); err != nil {
		return err
	}
	// Workers use this generation's own lifecycle socket, so an old worker still
	// finishing requests never reaches the new hostd, nor the new worker the old.
	private := socket + "." + strconv.Itoa(os.Getpid())
	template := workerStartRequest{Executable: executable, Version: buildinfo.Version, APIMin: 1, APIMax: 1, WorkerID: "runtime-" + strings.ReplaceAll(buildinfo.Version, " ", "-"), UID: os.Geteuid(), GID: os.Getegid(), HostdEndpoint: private, Capability: token, MutationsDisabled: true, Listener: listeners.runtime}
	owned := newOwnedWorkers(ctx, template, startWorker)

	var stopServer context.CancelFunc
	var serving []chan error
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		workerErr := owned.shutdown(shutdownCtx)
		var serverErr error
		if stopServer != nil {
			stopServer()
			for _, done := range serving {
				select {
				case err := <-done:
					serverErr = errors.Join(serverErr, err)
				case <-shutdownCtx.Done():
					serverErr = errors.Join(serverErr, runtimeContextFailure(shutdownCtx))
				}
			}
		}
		resultErr = withCleanup("hostd failed or its cleanup did not finish", resultErr, workerErr, serverErr, notifier.Stopping(), host.ShutdownHostd(shutdownCtx))
	}()

	maintenancePending, verifyCommit, err := nativeMaintenanceConfig(os.Getenv)
	if err != nil {
		return err
	}
	server, err := hostdproto.NewServer(hostdproto.SocketConfig{MaintenancePending: maintenancePending, VerifyMaintenanceCommitted: verifyCommit, SocketPath: socket, StatePath: filepath.Join(filepath.Dir(tokenPath), "hostd", "fence.json"), UID: os.Geteuid(), GID: os.Getegid(), Token: token, APIMin: 1, APIMax: 1, Workloads: host.WorkloadStatus, OpenSSH: host.OpenSSHStreams, UpdateGate: host.UpdateGate(), WorkerControl: owned, RequestTimeout: 31 * time.Minute})
	if err != nil {
		return err
	}
	if maintenancePending {
		stopCompletion := startMaintenanceCompletion(ctx, server)
		defer stopCompletion()
	}
	if err := listeners.bindPrivate(server, private, handover == nil); err != nil {
		return err
	}
	owner, _ := host.(*hostruntime.ProcessOwner)
	if owner != nil {
		owner.BindLifecycle(private+".workloads", token, server.Status, server.AcquireActive)
		if err := owner.StartBridge(ctx); err != nil {
			return err
		}
	}
	serverCtx, serverCancel := context.WithCancel(ctx)
	stopServer = serverCancel
	serve := func(listener *net.UnixListener) chan error {
		done := make(chan error, 1)
		// Closed after the result, so the shutdown wait never blocks on a
		// result the run loop already took.
		go func() { defer close(done); done <- server.Serve(serverCtx, listener) }()
		serving = append(serving, done)
		return done
	}
	serverDone := serve(listeners.private)
	if handover == nil {
		listeners.serveFixed(serve, owner)
	}
	_, err = owned.HandleWorkerControl(ctx, hostdproto.WorkerControlRequest{Operation: "start", WorkerID: template.WorkerID, Executable: executable, Version: template.Version, APIMin: 1, APIMax: 1})
	if err == nil {
		_, err = owned.HandleWorkerControl(ctx, hostdproto.WorkerControlRequest{Operation: "ready", WorkerID: template.WorkerID})
	}
	if err == nil {
		_, err = owned.HandleWorkerControl(ctx, hostdproto.WorkerControlRequest{Operation: "activate", WorkerID: template.WorkerID})
	}
	if err == nil && handover != nil {
		// The new worker already serves the shared runtime listener. The fixed
		// paths move last, so the old generation answers them until now.
		if err = listeners.commit(handover); err == nil {
			listeners.serveFixed(serve, owner)
		}
	}
	if err != nil {
		return err
	}
	if err := notifier.Ready(); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(output, "pb daemon hostd ready"); err != nil {
		return runtimeFailure("hostd readiness could not be written", err)
	}
	handedOver := make(chan error, 1)
	go func() {
		handedOver <- hostdproto.ServeHandover(serverCtx, listeners.handover, hostdproto.HandoverConfig{UID: os.Geteuid(), Token: token, Start: listeners.startFiles(), Commit: listeners.commitFiles()})
	}()
	watchdogInterval := notifier.WatchdogInterval()
	var watchdog <-chan time.Time
	if watchdogInterval > 0 {
		watchdogTicker := time.NewTicker(watchdogInterval)
		defer watchdogTicker.Stop()
		watchdog = watchdogTicker.C
	}
	for {
		select {
		case serverErr := <-serverDone:
			if ctx.Err() != nil && serverErr == nil {
				return runtimeFailureIfAny("hostd draining failed", runtimeShutdownCause(ctx), notifier.Draining())
			}
			return runtimeFailure("hostd lifecycle listener stopped", serverErr)
		case err := <-handedOver:
			if ctx.Err() != nil {
				return runtimeFailureIfAny("hostd draining failed", runtimeShutdownCause(ctx), notifier.Draining())
			}
			if err != nil {
				return runtimeFailure("hostd handover listener stopped", err)
			}
			// Teleport's parent Shutdown once the child signalled readiness
			// (lib/service/signals.go): stop accepting, drain, exit 0.
			listeners.stopAccepting()
			drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimewire.DrainLimit)
			defer cancel()
			return errors.Join(notifier.Draining(), owned.drain(drainCtx))
		case lost := <-owned.lost:
			// Without its worker the machine is not serving; Restart=always brings
			// hostd and a fresh worker back instead of waiting for an updater.
			return errors.Join(lost, notifier.Degraded("runtime worker exited"))
		case <-ctx.Done():
			return runtimeFailureIfAny("hostd draining failed", runtimeShutdownCause(ctx), notifier.Draining())
		case <-watchdog:
			if err := notifier.Watchdog(); err != nil {
				return errors.Join(err, notifier.Degraded("watchdog notification failed"))
			}
		}
	}
}

func runWorker(ctx context.Context, args []string, input io.Reader, output, stderr io.Writer) error {
	return runWorkerWith(ctx, args, input, output, stderr, newWorkerFeature)
}
func runWorkerWith(ctx context.Context, args []string, input io.Reader, output, stderr io.Writer, newFeature workerFeatureFactory) (resultErr error) {
	flags := flag.NewFlagSet("worker", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socket := flags.String("socket", "", "hostd lifecycle socket")
	tokenPath := flags.String("token-file", "", "hostd capability token")
	tokenFD := flags.Int("token-fd", -1, "hostd capability token descriptor")
	workerID := flags.String("worker-id", "", "runtime worker identity")
	version := flags.String("version", buildinfo.Version, "runtime version")
	apiMin := flags.Uint("api-min", 1, "minimum hostd API")
	apiMax := flags.Uint("api-max", 1, "maximum hostd API")
	heartbeat := flags.Duration("heartbeat", 5*time.Second, "hostd heartbeat interval")
	waitActivation := flags.Bool("wait-activation", false, "wait for private supervisor activation")
	if flags.Parse(args) != nil || flags.NArg() != 0 || !filepath.IsAbs(*socket) || (*tokenPath == "" && *tokenFD < 0) || (*tokenPath != "" && *tokenFD >= 0) || (*tokenPath != "" && !filepath.IsAbs(*tokenPath)) || *workerID == "" || *version == "" || *apiMin == 0 || *apiMin > 1024 || *apiMax < *apiMin || *apiMax > 1024 || *heartbeat < time.Second || *heartbeat > time.Minute {
		return errors.New("invalid worker invocation")
	}
	// SIGQUIT is hostd's graceful drain after a handover (nginx's QUIT).
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGQUIT)
	defer signal.Stop(quit)
	var token []byte
	var err error
	if *tokenPath != "" {
		token, err = readWorkerToken(*tokenPath)
	} else {
		token, err = readWorkerTokenFD(*tokenFD)
	}
	if err != nil {
		return err
	}
	client, err := hostdproto.NewClient(*socket, token, 5*time.Second, "")
	if err != nil {
		return err
	}
	candidate, err := hostdproto.NewCandidate(client, *workerID, *version, uint16(*apiMin), uint16(*apiMax))
	if err != nil {
		return err
	}
	ready, err := candidate.Ready(ctx)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "ready %d %d\n", ready.Epoch, ready.APIVersion); err != nil {
		return runtimeFailure("worker readiness could not be written", err)
	}
	if *waitActivation {
		if err := awaitWorkerActivation(ctx, input); err != nil {
			if runtimeCausesOnly(err, context.Canceled) {
				return nil
			}
			return err
		}
	}
	active, err := candidate.Activate(ctx)
	if err != nil {
		return err
	}
	feature, err := newFeature(ctx, *socket, token, *workerID, active.Epoch, *version)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if cleanupErr := feature.Shutdown(shutdownCtx); cleanupErr != nil {
			resultErr = withCleanup("runtime worker failed or its cleanup did not finish", resultErr, cleanupErr)
		}
	}()
	// Activation fences the lease before feature startup. Readiness means the
	// feature is usable and its heartbeat is fresh after that startup completes.
	if err := feature.Health(ctx); err != nil {
		return err
	}
	if err := candidate.Heartbeat(ctx); err != nil {
		return runtimeFailure("hostd worker heartbeat failed", err)
	}
	if _, err := fmt.Fprintf(output, "active %d %d\n", active.Epoch, active.APIVersion); err != nil {
		return runtimeFailure("hostd worker activation output could not be written", err)
	}
	ticker := time.NewTicker(*heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return runtimeShutdownCause(ctx)
		case <-quit:
			// A new hostd generation serves the shared runtime listener now:
			// stop accepting, finish in-flight requests, then shut down.
			drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimewire.DrainLimit)
			defer cancel()
			if draining, ok := feature.(interface{ DrainLocal(context.Context) error }); ok {
				return draining.DrainLocal(drainCtx)
			}
			return nil
		case <-ticker.C:
			if err := feature.Health(ctx); err != nil {
				return err
			}
			if err := candidate.Heartbeat(ctx); err != nil {
				return runtimeFailure("hostd worker heartbeat failed", err)
			}
		}
	}
}

func readWorkerTokenFD(descriptor int) ([]byte, error) {
	if descriptor < 3 || descriptor > 16 {
		return nil, errors.New("invalid worker token descriptor")
	}
	file := os.NewFile(uintptr(descriptor), "worker-token")
	if file == nil {
		return nil, errors.New("invalid worker token descriptor")
	}
	return readRuntimeToken(file)
}

func readWorkerToken(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return nil, runtimeFailure("invalid worker token file", err)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, runtimeFailure("opening the runtime token file failed", err)
	}
	return readRuntimeToken(file)
}
