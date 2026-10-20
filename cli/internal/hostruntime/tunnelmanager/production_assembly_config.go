package tunnelmanager

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connectorrotation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/networkrecovery"
)

var (
	// ErrProductionAssemblyInvalid means that a caller omitted one of the
	// server-owned identity, readiness, or transport boundaries. It is kept
	// separate from ErrInvalidConfig so production startup can report the
	// missing authority without silently falling back to the legacy connector.
	ErrProductionAssemblyInvalid        = errors.New("invalid production tunnel assembly")
	ErrProductionIdentityMissing        = errors.New("production tunnel identity source is unavailable")
	ErrProductionCredentialMissing      = errors.New("production tunnel credential renewal source is unavailable")
	ErrProductionControlMissing         = errors.New("production tunnel control stream source is unavailable")
	ErrProductionControlStarted         = errors.New("production tunnel control loop is already started")
	ErrProductionControlDisconnected    = errors.New("production tunnel control stream disconnected")
	ErrProductionControlRestartRequired = errors.New("production tunnel control requires a fresh authenticated session")
	// ErrConnectorGone means the control plane says the tunnel was deleted or the connector revoked.
	// It is terminal: the connector must not reconnect and its local record is removed.
	ErrConnectorGone = errors.New("tunnel connector no longer exists on the control plane")
)

// ProductionAssemblyConfig is the complete host-side connector-v1 assembly
// boundary. The server-owned identity and renewable credential sources are
// deliberately injected. This package must never derive a connector session
// or mint a credential from durable tunnel state.
//
// InitialConnector is the server-owned, write-only connector record used to
// seed a brand-new host-state store when the first snapshot arrives. It is
// never a credential or bearer token; the reference is resolved by the
// platform credential store at carrier setup time.
//
// ControlStream is an optional, already-authenticated connector-v1 bootstrap
// stream provider. It receives Hello/Welcome and the first snapshot before
// the data carrier exists. The protocol has no control migration message, so
// the stream remains the canonical control stream after activation.
type ProductionAssemblyConfig struct {
	Production ProductionConfig

	StableEndpointID string
	Clock            connectorprotocol.Clock
	SessionSource    connector.DataCarrierSessionSource
	// CarrierDescriptorSource resolves the carrier transport only after the
	// server-authenticated Welcome has supplied the live SessionID. It is the
	// production path for new enrollments. SessionSource remains supported for
	// already-running/recovered sessions; exactly one source is required.
	CarrierDescriptorSource    CarrierDescriptorSource
	Origins                    OriginProber
	OriginStreams              *OriginStreamForwarder
	OriginStreamsForGeneration func(ApplyRequest, connector.DataCarrierIdentity) *OriginStreamForwarder

	Control connectorrotation.ControlSessionConfig

	InitialConnector *hoststate.Connector

	// ControlSessionFactory creates a fresh ControlSession for each bootstrap
	// reconnect. ControlSession contains ClientSession state and cannot be
	// reused after a peer disconnects. The factory must obtain a new
	// server-authenticated Hello/Auth identity with a strictly higher
	// ProcessGeneration and re-sign the Auth transcript; a new Welcome session
	// ID alone is not sufficient for the server's durable stale-process fence.
	// This assembly only injects the durable applier and readiness boundary.
	ControlSessionFactory func(context.Context, *CoordinatedConfigApplier) (connectorrotation.ControlSessionConfig, error)
	// ControlStream is intentionally a one-shot, already-authenticated stream
	// provider. A replacement connector process must create a new assembly and
	// ControlSession with its new session/process identity.
	ControlStream  func(context.Context) (io.ReadWriteCloser, error)
	ObserveWelcome func(connectorprotocol.Welcome) error
	HelloRequestID string
	// OnConnectorGone runs once, on its own goroutine, when the control plane reports the connector
	// gone (ErrConnectorGone from the control stream). The owner removes the local connector.
	OnConnectorGone func()

	// NetworkReplacerFactory resolves an authenticated replacer for each
	// active durable connector. When omitted, this assembly supplies the one
	// connector represented by Control and rejects unrelated connectors rather
	// than inventing session or credential material for them.
	NetworkReplacerFactory NetworkRecoveryReplacerFactory
}

var ErrSessionStale = errors.New("tunnel control session is stale; reconnect required")

type CarrierDescriptorSource func(context.Context, connectorprotocol.Hello, connectorprotocol.Welcome, ApplyRequest) (connector.DataCarrierSessionSource, error)

// ProductionAssembly owns one ProductionManager, its coordinated applier,
// and one canonical connectorrotation.ControlSession. It implements the
// stable hostd tunnel workload boundary by delegating lifecycle and workload
// identity to the manager.
type ProductionAssembly struct {
	Manager *ProductionManager
	Factory *RuntimeFactory
	Applier *CoordinatedConfigApplier
	Control *connectorrotation.ControlSession

	controlRunner *connector.DataCarrierControlRunner
	// deferredCarrier is the Welcome-bound carrier source used by the
	// production bootstrap path. Its expected Hello must move with a fresh
	// reconnect process generation before the manager can prepare a carrier.
	deferredCarrier *welcomeDataCarrierSessionSource
	controlStream   func(context.Context) (io.ReadWriteCloser, error)
	observeWelcome  func(connectorprotocol.Welcome) error
	helloRequestID  string
	controlFactory  func(context.Context, *CoordinatedConfigApplier) (connectorrotation.ControlSessionConfig, error)
	accountID       string
	tunnelID        string
	connectorID     string
	hostID          string
	controlMu       sync.Mutex
	controlStarted  bool
	controlCancel   context.CancelFunc
	controlDone     chan struct{}
	controlErr      error
	activeControl   io.ReadWriteCloser
	activeSession   string
	networkRecovery *NetworkRecoveryCoordinator

	onGone   func()
	goneOnce sync.Once
	gone     chan struct{}
}

// ConnectorGone is closed when the control plane has reported this connector gone.
func (a *ProductionAssembly) ConnectorGone() <-chan struct{} { return a.gone }

func (a *ProductionAssembly) markConnectorGone() {
	a.goneOnce.Do(func() {
		close(a.gone)
		if a.onGone != nil {
			go a.onGone()
		}
	})
}

// OpenProductionAssembly builds the complete durable host-side composition.
// It does not open a second state store, create a second reconciliation loop,
// or activate a carrier. Activation remains the Manager's readiness-gated
// responsibility after the exact live connector identity has been supplied.
func OpenProductionAssembly(config ProductionAssemblyConfig) (*ProductionAssembly, hoststate.StartupStatus, error) {
	if err := validateProductionAssemblyConfig(config); err != nil {
		return nil, hoststate.StartupStatus{}, err
	}

	var sessionSource DataCarrierSessionSource
	var deferred *welcomeDataCarrierSessionSource
	if config.CarrierDescriptorSource != nil {
		deferred = &welcomeDataCarrierSessionSource{factory: config.CarrierDescriptorSource, hello: config.Control.Hello}
		sessionSource = deferred
	} else {
		sessionSource = liveDataCarrierSessionSource{source: config.SessionSource, expectedAccountID: config.Control.Hello.AccountID}
	}
	factory, err := NewRuntimeFactory(RuntimeFactoryConfig{Builder: DataCarrierBuilder{Sessions: sessionSource}, Origins: config.Origins, OriginStreams: config.OriginStreams, OriginStreamsForGeneration: config.OriginStreamsForGeneration})
	if err != nil {
		return nil, hoststate.StartupStatus{}, errors.Join(ErrProductionAssemblyInvalid, err)
	}
	production := config.Production
	production.Factory = factory
	if config.Clock != nil {
		production.Clock = config.Clock.Now
	}
	manager, status, err := OpenProduction(production)
	if err != nil {
		return nil, status, err
	}
	applier, err := manager.ConfigApplier(config.Clock, config.StableEndpointID)
	if err != nil {
		_ = manager.Shutdown(context.Background())
		return nil, status, errors.Join(ErrProductionAssemblyInvalid, err)
	}
	applier.InitialConnector = cloneInitialConnector(config.InitialConnector)
	networkFactory := config.NetworkReplacerFactory
	if networkFactory == nil {
		networkFactory = func(ctx context.Context, tunnel hoststate.Tunnel, connectorValue hoststate.Connector) (networkrecovery.Identity, networkrecovery.CarrierReplacer, error) {
			if ctx == nil || tunnel.ID != config.Control.Hello.TunnelID || connectorValue.ID != config.Control.Hello.ConnectorID || connectorValue.HostID != config.Control.Hello.HostID {
				return networkrecovery.Identity{}, nil, errors.Join(ErrProductionIdentityMissing, connectorprotocol.ErrIdentityMismatch)
			}
			identity := networkrecovery.Identity{EnvironmentID: config.Control.Hello.AccountID, MachineID: config.Control.Hello.HostID, TunnelID: tunnel.ID, ConnectorID: connectorValue.ID}
			replacer, replacerErr := NewNetworkCarrierReplacer(manager.Manager, identity)
			return identity, replacer, replacerErr
		}
	}
	networkRecovery, err := NewNetworkRecoveryCoordinator(NetworkRecoveryCoordinatorConfig{Manager: manager.Manager, Factory: networkFactory})
	if err != nil {
		_ = manager.Shutdown(context.Background())
		return nil, status, errors.Join(ErrProductionAssemblyInvalid, err)
	}

	controlConfig := config.Control
	controlConfig.Applier = applier
	controlConfig.SnapshotReadiness = applier
	control, err := connectorrotation.NewControlSession(controlConfig)
	if err != nil {
		_ = manager.Shutdown(context.Background())
		return nil, status, errors.Join(ErrProductionAssemblyInvalid, err)
	}
	runner, err := connector.NewDataCarrierControlRunner(connector.DataCarrierControlRunnerConfig{Control: control})
	if err != nil {
		_ = manager.Shutdown(context.Background())
		return nil, status, errors.Join(ErrProductionAssemblyInvalid, err)
	}
	observeWelcome := config.ObserveWelcome
	if deferred != nil {
		observeWelcome = func(welcome connectorprotocol.Welcome) error {
			if err := deferred.ObserveWelcome(welcome); err != nil {
				return err
			}
			if config.ObserveWelcome != nil {
				return config.ObserveWelcome(welcome)
			}
			return nil
		}
	}
	assembly := &ProductionAssembly{
		Manager:         manager,
		Factory:         factory,
		Applier:         applier,
		Control:         control,
		controlRunner:   runner,
		deferredCarrier: deferred,
		controlStream:   config.ControlStream,
		observeWelcome:  observeWelcome,
		helloRequestID:  config.HelloRequestID,
		controlFactory:  config.ControlSessionFactory,
		accountID:       config.Control.Hello.AccountID,
		tunnelID:        config.Control.Hello.TunnelID,
		connectorID:     config.Control.Hello.ConnectorID,
		hostID:          config.Control.Hello.HostID,
		networkRecovery: networkRecovery,
		onGone:          config.OnConnectorGone,
		gone:            make(chan struct{}),
	}
	if deferred != nil {
		deferred.reconnect = assembly.reconnectStaleSession
	}
	return assembly, status, nil
}

func validateProductionAssemblyConfig(config ProductionAssemblyConfig) error {
	if config.Production.Factory != nil || config.Production.StateRoot == "" || config.Production.HostID == "" || config.Production.Report == nil {
		return errors.Join(ErrProductionAssemblyInvalid, ErrInvalidConfig)
	}
	if err := hoststate.ValidateStableEndpointID(config.StableEndpointID); err != nil {
		return errors.Join(ErrProductionIdentityMissing, ErrProductionAssemblyInvalid, err)
	}
	if config.Clock == nil || config.Origins == nil {
		return errors.Join(ErrProductionAssemblyInvalid, ErrInvalidConfig)
	}
	if config.InitialConnector == nil {
		return errors.Join(ErrProductionIdentityMissing, ErrProductionAssemblyInvalid)
	}
	if (config.CarrierDescriptorSource == nil) == (config.SessionSource.Dialer == nil) {
		return errors.Join(ErrProductionIdentityMissing, ErrProductionAssemblyInvalid)
	}
	if config.CarrierDescriptorSource == nil {
		if err := validateConnectorSessionIdentity(config.SessionSource.Identity); err != nil {
			return errors.Join(ErrProductionIdentityMissing, err)
		}
		if config.SessionSource.Identity.HostID != config.Production.HostID {
			return errors.Join(ErrProductionIdentityMissing, ErrGenerationConflict)
		}
	}
	initial := *config.InitialConnector
	if initial.ID != config.Control.Hello.ConnectorID || initial.TunnelID != config.Control.Hello.TunnelID || initial.HostID != config.Control.Hello.HostID || initial.RotationGeneration == 0 || initial.RotationGeneration != config.Control.Hello.Auth.CredentialGeneration || initial.Credential.Generation != initial.RotationGeneration {
		return errors.Join(ErrProductionIdentityMissing, connectorprotocol.ErrIdentityMismatch)
	}
	if config.Control.Renewal == nil && config.Control.RenewalSigner == nil {
		return ErrProductionCredentialMissing
	}
	if config.ControlStream != nil && config.ControlSessionFactory == nil {
		return errors.Join(ErrProductionControlMissing, ErrProductionAssemblyInvalid)
	}
	if config.Control.Drainer == nil {
		return errors.Join(ErrProductionControlMissing, ErrProductionAssemblyInvalid)
	}
	if config.CarrierDescriptorSource == nil {
		if config.Control.Hello.AccountID != config.SessionSource.Identity.AccountID || config.Control.Hello.TunnelID != config.SessionSource.Identity.TunnelID || config.Control.Hello.ConnectorID != config.SessionSource.Identity.ConnectorID || config.Control.Hello.HostID != config.SessionSource.Identity.HostID || config.Control.Hello.ProcessGeneration != config.SessionSource.Identity.ProcessGeneration {
			return errors.Join(ErrProductionIdentityMissing, connectorprotocol.ErrIdentityMismatch)
		}
	} else if config.Control.Hello.HostID != config.Production.HostID {
		return errors.Join(ErrProductionIdentityMissing, connectorprotocol.ErrIdentityMismatch)
	}
	if err := config.Control.Hello.Validate(time.Time{}); err != nil {
		return errors.Join(ErrProductionControlMissing, err)
	}
	return nil
}

func cloneInitialConnector(value *hoststate.Connector) *hoststate.Connector {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func validateConnectorSessionIdentity(identity connector.DataCarrierIdentity) error {
	if identity.AccountID == "" || identity.HostID == "" || identity.TunnelID == "" || identity.ConnectorID == "" || identity.SessionID == "" || identity.ProcessGeneration == 0 || identity.Generation == 0 {
		return ErrProductionAssemblyInvalid
	}
	return nil
}
