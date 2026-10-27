//go:build windows

package localdaemon

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

func runWindowsDaemon(ctx context.Context, config DaemonConfig) (runErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if supportref.FromContext(ctx) == "" {
		ctx = supportref.WithContext(ctx, supportref.New())
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	if config.Source == nil || config.Paths.SocketPath == "" || config.Paths.LockPath == "" || config.Paths.StateRoot == "" {
		return ErrInvalidInventoryConfig
	}
	ownerSID, err := resolveWindowsOwnerSID(config.OwnerSID)
	if err != nil {
		return ErrInvalidInventoryConfig
	}
	lock, err := acquireProcessLock(config.Paths.LockPath, ownerSID)
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
		recorder, err = diagnostics.NewRecorder(diagnostics.DiskConfig{
			Directory: filepath.Join(config.Paths.StateRoot, "diagnostics"),
			OwnerSID:  ownerSID,
			OwnerUID:  -1,
			Clock:     config.Clock,
		})
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

	// Managed SSH starts from the first inventory refresh, after the local API serves (managedSSHOwner).
	var managedSSH *managedSSHOwner
	if config.ManagedSSH != nil {
		managedSSH = &managedSSHOwner{lifetime: ctx, config: *config.ManagedSSH, start: StartManagedSSH, report: func(reportCtx context.Context, startErr error) {
			reportManagedSSHStartup(reportCtx, config.Source, recorder, startErr)
		}}
		defer func() { runErr = daemonCleanupFailure(runErr, managedSSH.close()) }()
	}

	observeSSHRefresh := maintenanceObserver(ctx, recorder, "managed_ssh")
	observePeerMetadata := maintenanceObserver(ctx, recorder, "peer_metadata")
	diagnosticClock := config.Clock
	if diagnosticClock == nil {
		diagnosticClock = time.Now
	}
	// Publish local readiness before the first control-plane refresh. A refresh
	// has a bounded network timeout and must not keep the named pipe absent long
	// enough for the first command after logon or reboot to fail. Inventory.Run
	// replaces this starting snapshot as soon as reconciliation completes.
	store, err := localapi.NewSnapshotStore(&localapi.Snapshot{
		Schema:        localapi.SnapshotSchemaV1,
		Generation:    1,
		ObservedAt:    diagnosticClock().UTC(),
		DaemonState:   "starting",
		DaemonVersion: buildinfo.Version,
	})
	if err != nil {
		return err
	}
	diagnosticAPI := &diagnosticService{
		recorder:  recorder,
		snapshots: store,
		stateRoot: config.Paths.StateRoot,
		ownerSID:  ownerSID,
		clock:     diagnosticClock,
	}
	inventory, err := NewInventory(InventoryConfig{
		Source:            config.Source,
		Store:             store,
		RefreshInterval:   config.RefreshInterval,
		RequestTimeout:    config.RequestTimeout,
		Clock:             config.Clock,
		ReconcileMachines: config.ReconcileMachines,
		Events:            config.InventoryEvents,
		AccessEvents:      config.AccessEvents,
		OnRefresh:         inventoryRefreshObserver(ctx, recorder), OnMachines: func(refreshCtx context.Context, machines []api.UserMachine) {
			if config.OnMachines != nil {
				config.OnMachines(refreshCtx, machines)
			}
			authorityInvalidator.Observe(machines)
			if managedSSH != nil {
				sshCtx, cancelSSH := context.WithTimeout(refreshCtx, 15*time.Second)
				observeSSHRefresh(managedSSH.refresh(sshCtx, machines))
				publishSSHDomain(store, managedSSH.domain())
				cancelSSH()
			}
			if config.WarmPeerMetadata == nil {
				return
			}
			warmTimeout := config.RequestTimeout
			if warmTimeout <= 0 {
				warmTimeout = defaultRequestTimeout
			}
			warmCtx, cancelWarm := context.WithTimeout(refreshCtx, warmTimeout)
			warmErr := config.WarmPeerMetadata(warmCtx, machines)
			cancelWarm()
			observePeerMetadata(warmErr)
		},
	})
	if err != nil {
		return err
	}
	if managedSSH != nil {
		managedSSH.onStarted = inventory.RequestRefresh
	}
	observations, err := NewObservationStore(ObservationConfig{Store: store, OwnerUID: -1, OwnerSID: ownerSID, Clock: config.Clock})
	if err != nil {
		return err
	}
	network, stopNetwork, err := startNetworkStatus(inventory.RequestRefresh)
	if err != nil {
		return err
	}
	defer stopNetwork()
	source := snapshotSource{Inventory: inventory, denial: config.RelayDenial, transport: config.NativeTransport, network: network}
	diagnosticAPI.snapshots = source
	vaultCustody := localapi.NewVaultCustodyStore()
	defer func() { runErr = daemonCleanupFailure(runErr, vaultCustody.Close()) }()
	server, err := localapi.NewServer(localapi.ServerConfig{
		SocketPath:           config.Paths.SocketPath,
		OwnerUID:             -1,
		OwnerGID:             -1,
		OwnerSID:             ownerSID,
		Source:               source,
		Completions:          inventory,
		Diagnostics:          diagnosticAPI,
		AuthorizeDiagnostics: func(peer localapi.Peer) bool { return peer.SID == ownerSID },
		Observations:         observations,
		PeerStreams:          peerStreamBroker{open: config.OpenPeerStream, issue: config.IssuePeerStream},
		PeerProbes:           peerProbeBroker{probe: config.ProbePeer},
		InspectorRequests:    inspectorBroker(config.InspectorRequest),
		RelayInventory:       config.RelayInventory,
		FileTransfers:        config.FileTransfers,
		VaultCustody:         vaultCustody,
		Stale:                lock,
	})
	if err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, 3)
	go func() { results <- server.Run(runCtx) }()
	go func() { results <- inventory.Run(runCtx) }()
	go func() { results <- observations.Run(runCtx) }()
	first := <-results
	cancel()
	_ = vaultCustody.Close()
	remaining := []error{<-results, <-results}
	return errors.Join(first, remaining[0], remaining[1], ctx.Err())
}

type peerStreamBroker struct {
	open  func(context.Context, localapi.Peer, localapi.PeerStreamRequest) (net.Conn, error)
	issue func(context.Context, localapi.PeerStreamRequest) (localapi.PeerStreamRequest, error)
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

type peerProbeBroker struct {
	probe func(context.Context, localapi.Peer, localapi.PeerStreamRequest) (localapi.PeerProbeResult, error)
}

func (b peerProbeBroker) ProbePeer(ctx context.Context, peer localapi.Peer, request localapi.PeerStreamRequest) (localapi.PeerProbeResult, error) {
	if b.probe == nil {
		return localapi.PeerProbeResult{}, errors.New("peer probe broker is unavailable")
	}
	return b.probe(ctx, peer, request)
}

var _ localapi.PeerStreamBroker = peerStreamBroker{}
var _ localapi.PeerProbeBroker = peerProbeBroker{}
