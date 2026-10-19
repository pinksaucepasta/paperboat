package connectorrotation

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
)

var ErrControlSessionInvalid = errors.New("invalid connector control session")

var ErrControlSessionOutboundFull = errors.New("connector control outbound queue is full")

const (
	defaultControlOutboundQueue = 16
	maxControlOutboundQueue     = 1024
	defaultRenewalLead          = 30 * time.Second
	readinessCleanupTimeout     = 250 * time.Millisecond
)

// ReplacementReadiness is the host-side observation needed before a new
// credential can become authoritative. The provider must start or select the
// replacement process, wait for its connector, route, and origin readiness,
// and return the exact replacement session identity. It must not report a
// successful result based only on local configuration state.
type ReplacementReadiness struct {
	// Session is the replacement ClientSession that has accepted its Welcome,
	// applied the candidate through its ConfigApplier (normally
	// connectorprotocol.HostStateApplier), and reached SessionReady. It is
	// required. The metadata fields below are an observation from the process
	// supervisor and are checked against this session and its active snapshot.
	Session                *connectorprotocol.ClientSession
	NegotiatedCapabilities []string
	SessionID              string
	ProcessGeneration      uint64
	ConfigGeneration       uint64
	ConfigContentHash      string
	EdgeReady              bool
	RouteReady             bool
	OriginReady            bool
}

// ReadinessSource bridges the host process supervisor to the control
// protocol. The source owns process replacement and data-plane observation;
// this package only validates its result and emits the bound ready message.
type ReadinessSource interface {
	WaitReplacementReady(context.Context, connectorprotocol.CredentialRotationInstall) (ReplacementReadiness, error)
}

// SnapshotReadinessSource is the regular configuration cutover boundary. It
// must return only after the exact staged snapshot has an active edge carrier,
// admitted routes, and usable origins. A transport ping or durable state write
// is not sufficient to satisfy this contract.
type SnapshotReadinessSource interface {
	WaitReady(context.Context, connectorprotocol.Snapshot) (connectorprotocol.Readiness, error)
}

// CredentialRenewalSource supplies fresh proof material before the current
// credential expires. The control session owns when the request is framed and
// written; the source owns credential custody and signing.
type CredentialRenewalSource interface {
	Renew(context.Context, time.Time) (nonce, signedProof string, err error)
}

type CredentialRenewalSourceFunc func(context.Context, time.Time) (string, string, error)

func (f CredentialRenewalSourceFunc) Renew(ctx context.Context, now time.Time) (string, string, error) {
	return f(ctx, now)
}

// RenewalProofSigner keeps connector private bytes behind a reference-backed
// signing boundary. ControlSession constructs the complete renewal transcript
// only after Welcome has supplied the live SessionID, then passes the exact
// canonical payload here. A signer must return raw Ed25519 signature bytes.
type RenewalProofSigner interface {
	SignRenewalProof(context.Context, []byte) ([]byte, error)
}

type RenewalProofSignerFunc func(context.Context, []byte) ([]byte, error)

func (f RenewalProofSignerFunc) SignRenewalProof(ctx context.Context, payload []byte) ([]byte, error) {
	return f(ctx, payload)
}

// RotationRevokeCommitter retires the old process only after ControlSession
// has successfully written the server-authoritative revoked acknowledgement.
type RotationRevokeCommitter interface {
	CommitRevoke(context.Context, connectorprotocol.CredentialRotationRevoke) error
}

// RotationRevokePreparer durably commits the replacement credential after
// the server has instructed revoke but before the terminal revoked
// acknowledgement is written. A crash in that window must reconnect with the
// new credential rather than a server-revoked old reference.
type RotationRevokePreparer interface {
	PrepareRevoke(context.Context, connectorprotocol.CredentialRotationRevoke) error
}

// RotationRevokeRejoiner marks a successful revoke as a control-session
// identity boundary. Serve writes the revoked acknowledgement first, then
// exits cleanly so the production supervisor reconnects with the newly
// authoritative credential and a fresh rotation journal.
type RotationRevokeRejoiner interface {
	RejoinAfterRevoke() bool
}

// ControlSession is the host-side connector-v1 adapter. It combines the
// shared ClientSession state machine with the host credential-rotation Manager
// and exposes a framed Serve loop for the transport owner. The transport is
// deliberately an io.ReadWriteCloser so TRK-14 can supply QUIC/TCP/Unix
// carriers without moving credential custody into this package.
type ControlSession struct {
	mu                sync.RWMutex
	client            *connectorprotocol.ClientSession
	rotation          *Manager
	readiness         ReadinessSource
	snapshotReadiness SnapshotReadinessSource
	renewal           CredentialRenewalSource
	renewalSigner     RenewalProofSigner
	renewalLead       time.Duration
	outbound          chan connectorprotocol.Frame
	clock             connectorprotocol.Clock
	welcomed          bool
	welcome           connectorprotocol.Welcome
	requestSeq        uint64
	automaticRotation bool
	revokeCommitter   RotationRevokeCommitter
	rotationReadyOps  map[string]struct{}
}

type ControlSessionConfig struct {
	Hello                      connectorprotocol.Hello
	Applier                    connectorprotocol.ConfigApplier
	Drainer                    connectorprotocol.Drainer
	Rotation                   *Manager
	Readiness                  ReadinessSource
	SnapshotReadiness          SnapshotReadinessSource
	Renewal                    CredentialRenewalSource
	RenewalSigner              RenewalProofSigner
	AutomaticRotationReadiness bool
	RotationRevokeCommitter    RotationRevokeCommitter
	RenewalLead                time.Duration
	OutboundQueue              int
	Clock                      connectorprotocol.Clock
	ApplyTimeout               time.Duration
	AbortTimeout               time.Duration
}

type controlRealClock struct{}

func (controlRealClock) Now() time.Time { return time.Now().UTC() }
