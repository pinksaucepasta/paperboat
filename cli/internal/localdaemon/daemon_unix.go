//go:build darwin || linux

package localdaemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type DaemonConfig struct {
	Paths                   localapi.Paths
	Source                  MachineSource
	OwnerUID                int
	OwnerGID                int
	RefreshInterval         time.Duration
	RequestTimeout          time.Duration
	Clock                   func() time.Time
	ManagedSSH              *ManagedSSHConfig
	OpenPeerStream          func(context.Context, localapi.Peer, localapi.PeerStreamRequest) (net.Conn, error)
	ProbePeer               func(context.Context, localapi.Peer, localapi.PeerStreamRequest) (localapi.PeerProbeResult, error)
	RelayInventory          func(context.Context) (localapi.RelayInventory, error)
	InvalidatePeerAuthority func(string)
	WarmPeerMetadata        func(context.Context, []api.UserMachine) error
	IssuePeerStream         func(context.Context, localapi.PeerStreamRequest) (localapi.PeerStreamRequest, error)
	FileTransfers           localapi.FileTransferBroker
	ReconcileMachines       func(context.Context, []api.UserMachine) error
	OnMachines              func(context.Context, []api.UserMachine)
}

func Run(ctx context.Context, config DaemonConfig) (runErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if supportref.FromContext(ctx) == "" {
		ctx = supportref.WithContext(ctx, supportref.New())
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	if config.Source == nil || config.OwnerUID < 0 || config.OwnerGID < 0 || config.Paths.SocketPath == "" || config.Paths.LockPath == "" {
		return ErrInvalidInventoryConfig
	}
	lock, err := acquireProcessLock(config.Paths.LockPath, config.OwnerUID)
	if err != nil {
		return err
	}
	defer func() { runErr = daemonCleanupFailure(runErr, lock.Close()) }()
	if closer, ok := config.FileTransfers.(interface{ Close() error }); ok {
		defer func() { runErr = daemonCleanupFailure(runErr, closer.Close()) }()
	}
	recorder := diagnostics.FromContext(ctx)
	ownedRecorder := recorder == nil
	if ownedRecorder {
		recorder, err = diagnostics.NewRecorder(diagnostics.DiskConfig{Directory: filepath.Join(config.Paths.StateRoot, "diagnostics"), OwnerUID: config.OwnerUID, Clock: config.Clock})
		if err != nil {
			return err
		}
	}
	ctx = diagnostics.WithRecorder(ctx, recorder)
	authorityInvalidator := NewMachineAuthorityInvalidator(config.InvalidatePeerAuthority)
	reference := supportref.FromContext(ctx)
	if err := recorder.RecordWithSupportReference("daemon", "lifecycle", "info", reference, map[string]string{"state": "starting"}); err != nil {
		if ownedRecorder {
			err = errors.Join(err, recorder.Close())
		}
		return err
	}
	defer func() {
		runErr = daemonCleanupFailure(runErr, recorder.RecordWithSupportReference("daemon", "lifecycle", "info", reference, map[string]string{"state": "stopping"}))
		if ownedRecorder {
			runErr = daemonCleanupFailure(runErr, recorder.Close())
		}
	}()
	var managedSSHRuntime *ManagedSSHRuntime
	if config.ManagedSSH != nil {
		managedSSHRuntime, err = StartManagedSSH(ctx, *config.ManagedSSH)
		reportManagedSSHStartup(ctx, config.Source, recorder, err)
		defer func() {
			if managedSSHRuntime != nil {
				runErr = daemonCleanupFailure(runErr, managedSSHRuntime.Close())
			}
		}()
	}

	observeSSHRefresh := maintenanceObserver(ctx, recorder, "managed_ssh")
	observePeerMetadata := maintenanceObserver(ctx, recorder, "peer_metadata")
	diagnosticClock := config.Clock
	if diagnosticClock == nil {
		diagnosticClock = time.Now
	}
	// Local diagnostics must be available while remote inventory is reconciling.
	store, err := localapi.NewSnapshotStore(&localapi.Snapshot{
		Schema: localapi.SnapshotSchemaV1, Generation: 1,
		ObservedAt: diagnosticClock().UTC(), DaemonState: "starting", DaemonVersion: buildinfo.Version,
	})
	if err != nil {
		return err
	}
	diagnosticAPI := &diagnosticService{recorder: recorder, store: store, stateRoot: config.Paths.StateRoot, ownerUID: config.OwnerUID, clock: diagnosticClock}
	inventory, err := NewInventory(InventoryConfig{Source: config.Source, Store: store, RefreshInterval: config.RefreshInterval, RequestTimeout: config.RequestTimeout, Clock: config.Clock, ReconcileMachines: config.ReconcileMachines, OnRefresh: inventoryRefreshObserver(ctx, recorder), OnMachines: func(refreshCtx context.Context, machines []api.UserMachine) {
		if config.OnMachines != nil {
			config.OnMachines(refreshCtx, machines)
		}
		authorityInvalidator.Observe(machines)
		if managedSSHRuntime != nil {
			sshCtx, cancelSSH := context.WithTimeout(refreshCtx, 15*time.Second)
			observeSSHRefresh(managedSSHRuntime.Refresh(sshCtx))
			cancelSSH()
		}
		if config.WarmPeerMetadata != nil {
			warmTimeout := config.RequestTimeout
			if warmTimeout <= 0 {
				warmTimeout = defaultRequestTimeout
			}
			warmCtx, cancelWarm := context.WithTimeout(refreshCtx, warmTimeout)
			warmErr := config.WarmPeerMetadata(warmCtx, machines)
			cancelWarm()
			observePeerMetadata(warmErr)
		}
	}})
	if err != nil {
		return err
	}
	observations, err := NewObservationStore(ObservationConfig{Store: store, OwnerUID: config.OwnerUID, Clock: config.Clock})
	if err != nil {
		return err
	}
	server, err := localapi.NewServer(localapi.ServerConfig{
		SocketPath:           config.Paths.SocketPath,
		OwnerUID:             config.OwnerUID,
		OwnerGID:             config.OwnerGID,
		Source:               store,
		Completions:          inventory,
		Diagnostics:          diagnosticAPI,
		AuthorizeDiagnostics: func(peer localapi.Peer) bool { return peer.UID == config.OwnerUID },
		Observations:         observations,
		PeerStreams:          peerStreamBroker{open: config.OpenPeerStream, issue: config.IssuePeerStream},
		PeerProbes:           peerProbeBroker{probe: config.ProbePeer},
		RelayInventory:       config.RelayInventory,
		FileTransfers:        config.FileTransfers,
		Stale:                lock,
	})
	if err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, 3)
	go func() { results <- server.Run(runCtx) }()
	go func() {
		_ = inventory.Refresh(runCtx)
		if runCtx.Err() != nil {
			results <- runCtx.Err()
			return
		}
		results <- inventory.runTicker(runCtx)
	}()
	go func() { results <- observations.Run(runCtx) }()
	first := <-results
	cancel()
	remaining := []error{<-results, <-results}
	return errors.Join(first, remaining[0], remaining[1], ctx.Err())
}

type peerStreamBroker struct {
	open  func(context.Context, localapi.Peer, localapi.PeerStreamRequest) (net.Conn, error)
	issue func(context.Context, localapi.PeerStreamRequest) (localapi.PeerStreamRequest, error)
}

type peerProbeBroker struct {
	probe func(context.Context, localapi.Peer, localapi.PeerStreamRequest) (localapi.PeerProbeResult, error)
}

func (b peerProbeBroker) ProbePeer(ctx context.Context, peer localapi.Peer, request localapi.PeerStreamRequest) (localapi.PeerProbeResult, error) {
	if b.probe == nil {
		return localapi.PeerProbeResult{}, errors.New("peer probe broker is unavailable")
	}
	return b.probe(ctx, peer, request)
}

func (b peerStreamBroker) OpenPeerStream(ctx context.Context, peer localapi.Peer, request localapi.PeerStreamRequest) (net.Conn, error) {
	if request.Credential == "" {
		if b.issue == nil {
			return nil, ErrInvalidInventoryConfig
		}
		var err error
		request, err = b.issue(ctx, request)
		if err != nil {
			return nil, err
		}
	}
	if b.open == nil {
		return nil, errors.New("peer stream broker is unavailable")
	}
	return b.open(ctx, peer, request)
}

func CurrentUserPaths() (localapi.Paths, error) {
	return localapi.CurrentPaths(os.Geteuid())
}
