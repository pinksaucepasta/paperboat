//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	hostruntime "github.com/pinksaucepasta/paperboat/internal/hostruntime/runtime"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

type hostdHost interface {
	StartHostd(context.Context) error
	ShutdownHostd(context.Context) error
	WorkloadStatus() hostdproto.WorkloadStatus
	UpdateGate() hostdproto.UpdateGateHandler
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
	host, err := newHost(ctx, buildinfo.Version, os.Getenv)
	if err != nil {
		return runtimeFailure("hostd initialization failed", err, notifier.Degraded("hostd initialization failed"))
	}
	if err := host.StartHostd(ctx); err != nil {
		return runtimeFailure("hostd startup failed", err, notifier.Degraded("hostd startup failed"))
	}
	template := workerStartRequest{Executable: executable, Version: buildinfo.Version, APIMin: 1, APIMax: 1, WorkerID: "runtime-" + strings.ReplaceAll(buildinfo.Version, " ", "-"), UID: os.Geteuid(), GID: os.Getegid(), HostdEndpoint: socket, Capability: token, MutationsDisabled: true}
	owned := newOwnedWorkers(ctx, template, startWorker)

	var stopServer context.CancelFunc
	var serverDone chan error
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		workerErr := owned.shutdown(shutdownCtx)
		var serverErr error
		if stopServer != nil {
			stopServer()
			select {
			case serverErr = <-serverDone:
			case <-shutdownCtx.Done():
				serverErr = runtimeContextFailure(shutdownCtx)
			}
		}
		resultErr = withCleanup("hostd failed or its cleanup did not finish", resultErr, workerErr, serverErr, notifier.Stopping(), host.ShutdownHostd(shutdownCtx))
	}()

	var drainExecutions func(context.Context) error
	if drainer, ok := host.(interface{ DrainExecutions(context.Context) error }); ok {
		drainExecutions = drainer.DrainExecutions
	}
	maintenancePending, verifyCommit, err := nativeMaintenanceConfig(os.Getenv)
	if err != nil {
		return err
	}
	server, err := hostdproto.NewServer(hostdproto.SocketConfig{MaintenancePending: maintenancePending, VerifyMaintenanceCommitted: verifyCommit, SocketPath: socket, StatePath: filepath.Join(filepath.Dir(tokenPath), "hostd", "fence.json"), UID: os.Geteuid(), GID: os.Getegid(), Token: token, APIMin: 1, APIMax: 1, Workloads: host.WorkloadStatus, DrainExecutions: drainExecutions, UpdateGate: host.UpdateGate(), WorkerControl: owned, RequestTimeout: 31 * time.Minute})
	if err != nil {
		return err
	}
	if maintenancePending {
		stopCompletion := startMaintenanceCompletion(ctx, server)
		defer stopCompletion()
	}
	if owner, ok := host.(*hostruntime.ProcessOwner); ok {
		owner.BindLifecycle(socket+".workloads", token, server.Status, server.AcquireActive)
		if err := owner.StartBridge(ctx); err != nil {
			return err
		}
	}
	serverCtx, serverCancel := context.WithCancel(ctx)
	stopServer = serverCancel
	serverDone = make(chan error, 1)
	go func() { defer close(serverDone); serverDone <- server.Run(serverCtx) }()
	if err := waitForHostdSocket(ctx, socket, serverDone); err != nil {
		stopServer()
		return err
	}
	_, err = owned.HandleWorkerControl(ctx, hostdproto.WorkerControlRequest{Operation: "start", WorkerID: template.WorkerID, Executable: executable, Version: template.Version, APIMin: 1, APIMax: 1})
	if err == nil {
		_, err = owned.HandleWorkerControl(ctx, hostdproto.WorkerControlRequest{Operation: "ready", WorkerID: template.WorkerID})
	}
	if err == nil {
		_, err = owned.HandleWorkerControl(ctx, hostdproto.WorkerControlRequest{Operation: "activate", WorkerID: template.WorkerID})
	}
	if err != nil {
		stopServer()
		return err
	}
	if err := notifier.Ready(); err != nil {
		stopServer()
		return err
	}
	if _, err := fmt.Fprintln(output, "pb daemon hostd ready"); err != nil {
		return runtimeFailure("hostd readiness could not be written", err)
	}
	watchdogInterval := notifier.WatchdogInterval()
	var watchdog <-chan time.Time
	var watchdogTicker *time.Ticker
	if watchdogInterval > 0 {
		watchdogTicker = time.NewTicker(watchdogInterval)
		defer watchdogTicker.Stop()
		watchdog = watchdogTicker.C
	}
	var runErr error
run:
	for {
		select {
		case serverErr := <-serverDone:
			if ctx.Err() != nil && serverErr == nil {
				runErr = runtimeFailureIfAny("hostd draining failed", runtimeShutdownCause(ctx), notifier.Draining())
			} else {
				runErr = runtimeFailure("hostd lifecycle listener stopped", serverErr)
			}
			break run
		case <-ctx.Done():
			runErr = runtimeFailureIfAny("hostd draining failed", runtimeShutdownCause(ctx), notifier.Draining())
			break run
		case <-watchdog:
			if err := notifier.Watchdog(); err != nil {
				runErr = errors.Join(err, notifier.Degraded("watchdog notification failed"))
				break run
			}
		}
	}
	return runErr
}

func waitForHostdSocket(ctx context.Context, socket string, done <-chan error) error {
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	var lastErr error
	for {
		info, err := os.Lstat(socket)
		if err == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		if err != nil && !runtimeCausesOnly(err, os.ErrNotExist) {
			lastErr = err
		}
		if err == nil {
			lastErr = runtimeFailure("hostd lifecycle socket has an invalid type")
		}

		select {
		case err := <-done:
			return runtimeFailure("hostd lifecycle listener exited before readiness", err, lastErr)
		case <-ctx.Done():
			return runtimeFailure("hostd lifecycle readiness canceled", runtimeContextFailure(ctx), lastErr)
		case <-deadline.C:
			return runtimeFailure("hostd lifecycle socket did not become ready", context.DeadlineExceeded, lastErr)
		case <-tick.C:
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
	client, err := hostdproto.NewClient(*socket, token, 5*time.Second)
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
