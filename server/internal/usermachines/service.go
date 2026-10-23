package usermachines

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/access"
	"github.com/pinksaucepasta/paperboat-server/internal/accessdescriptor"
	"github.com/pinksaucepasta/paperboat-server/internal/accountingauthority"
	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/helperruntime"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
	"github.com/pinksaucepasta/paperboat-server/internal/secrets"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
	"github.com/pinksaucepasta/paperboat-server/internal/teaminbox"
	"github.com/pinksaucepasta/paperboat-server/internal/workers"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

var (
	ErrInvalidPairing                       = errors.New("invalid user-machine pairing")
	ErrInvalidSetup                         = errors.New("invalid machine setup")
	ErrInvalidMachineInstallation           = errors.New("invalid authenticated machine installation")
	ErrMachineInstallationOperationConflict = errors.New("authenticated machine installation operation conflicts with an existing request")
	ErrMachineIdentityConflict              = errors.New("machine identity belongs to another account")
	ErrMachineNameConflict                  = errors.New("machine name is already in use")
	ErrInvalidMachineName                   = errors.New("invalid machine name")
	ErrPairingExpired                       = errors.New("user-machine pairing expired")
	ErrPairingUsed                          = errors.New("user-machine pairing is no longer pending")
	ErrSeatUnavailable                      = errors.New("user-machine seat unavailable")
	ErrNotFound                             = errors.New("user machine not found")
	ErrBandwidthDenied                      = errors.New("user-machine bandwidth is unavailable")
	ErrInvalidBandwidth                     = errors.New("user-machine bandwidth request is invalid")
	ErrInstallationPending                  = errors.New("user-machine installation approval is pending")
	ErrInstallationDenied                   = errors.New("user-machine installation was denied")
	ErrInstallationExpired                  = errors.New("user-machine installation pairing expired")
	ErrInstallationUnavailable              = errors.New("user-machine installation material is unavailable")
	ErrProvisioningUnavailable              = errors.New("user-machine canonical helper provisioning is unavailable")
	ErrEnrollmentNotFound                   = errors.New("user-machine enrollment not found")
	ErrEnrollmentTokenExpired               = errors.New("machine enrollment token expired")
	ErrEnrollmentTokenUsed                  = errors.New("machine enrollment token already claimed")
	ErrEnrollmentTokenInvalid               = errors.New("machine enrollment token invalid or revoked")
	ErrPairingPlatformUnsupported           = errors.New("machine pairing platform is unsupported")
	ErrEnrollmentState                      = errors.New("user-machine enrollment state does not allow this operation")
	ErrIdempotencyKeyRequired               = errors.New("user-machine enrollment idempotency key is required")
	ErrWorkspaceForbidden                   = errors.New("machines cannot be added to that workspace")
	ErrTerminalSessionNotFound              = errors.New("user-machine terminal session not found")
	ErrTerminalSessionEnded                 = errors.New("user-machine terminal session has closed")
	ErrTerminalSharingNotApplied            = errors.New("terminal sharing was not applied")
	ErrTerminalSessionReserved              = errors.New("user-machine default terminal session is reserved")
	ErrTerminalSessionConflict              = errors.New("user-machine terminal session name conflict")
	ErrTerminalSessionNotClosed             = errors.New("user-machine terminal session is not closed")
	ErrTerminalSessionInvalidName           = errors.New("invalid user-machine terminal session name")
	ErrTerminalSessionInvalidCWD            = errors.New("invalid user-machine terminal session cwd")
	ErrTerminalSessionIdempotency           = errors.New("terminal session idempotency key is required")
	ErrTransferDestinationInvalid           = errors.New("transfer destination is unavailable")
	ErrMachineCapabilityUnavailable         = errors.New("machine capability is unavailable")
	ErrMachineOffline                       = errors.New("machine is offline")
	ErrExecOperationInvalid                 = errors.New("exec operation id is invalid")
	ErrSSHOperationInvalid                  = errors.New("ssh operation id is invalid")
)

func exactNoRows(err error) bool {
	return err == sql.ErrNoRows || err == pgx.ErrNoRows
}

var terminalSessionNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var execOperationIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

// A consumed pairing may be recovered only for a short, bounded period after
// its normal expiry. The local journal and the original machine public key
// are required for that recovery; an old verifier is never an unbounded
// installation credential.
const installationRecoveryGrace = 24 * time.Hour
const terminalSessionClosedRecordRetention = 30 * 24 * time.Hour

func installationRecoveryOperationKey(pairingID string, expiresAt time.Time) string {
	return fmt.Sprintf("machine-recovery:%s:%d", pairingID, expiresAt.UTC().UnixNano())
}

type SeatAuthorizer interface {
	ReserveUserMachineSeat(context.Context, *db.Tx, string) error
}

func (s *Service) ConsumeInstallation(ctx context.Context, verifier string) (json.RawMessage, error) {
	return s.consumeInstallation(ctx, verifier, "", false)
}

// ConsumeInstallationForIdentity is the current bootstrap exchange. The
// identity is checked before consuming or replaying material and is also the
// binding used by the post-expiry recovery path.
func (s *Service) ConsumeInstallationForIdentity(ctx context.Context, verifier, publicIdentityKey string) (json.RawMessage, error) {
	return s.consumeInstallation(ctx, verifier, strings.TrimSpace(publicIdentityKey), false)
}

// ConsumeInstallationForIdentityState is the verifier-bound recovery exchange.
// runtimeEnrolled is a journal assertion: only a machine that has already
// persisted its runtime identity may reuse the active server helper.
func (s *Service) ConsumeInstallationForIdentityState(ctx context.Context, verifier, publicIdentityKey string, runtimeEnrolled bool) (json.RawMessage, error) {
	return s.consumeInstallation(ctx, verifier, strings.TrimSpace(publicIdentityKey), runtimeEnrolled)
}

func (s *Service) consumeInstallation(ctx context.Context, verifier, publicIdentityKey string, runtimeEnrolled bool) (material json.RawMessage, err error) {
	defer func() {
		if err == nil && runtimeEnrolled {
			material, err = s.runtimeRecoveryMaterial(ctx, verifier, publicIdentityKey, material)
		}
	}()
	if strings.TrimSpace(verifier) == "" || strings.TrimSpace(s.encryptionKey) == "" {
		return nil, ErrInstallationUnavailable
	}
	hash := sha256.Sum256([]byte(verifier))
	now := s.now().UTC()
	var ciphertext []byte
	var recoveryPairing dbsqlc.UserMachinePairing
	var recoveryUserID string
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		value, err := tx.Queries().ConsumeUserMachineInstallationConfig(ctx, dbsqlc.ConsumeUserMachineInstallationConfigParams{VerifierHash: hash[:], PublicIdentityKey: publicIdentityKey})
		if errors.Is(err, sql.ErrNoRows) {
			// A successful response can be lost after the database transaction
			// commits but before the installer persists its local resume state.
			// Permit the same verifier to recover the encrypted material while
			// its enrollment is still installing. The identity binding is
			// optional only for legacy clients during the original expiry window.
			value, replayErr := tx.Queries().GetUserMachineInstallationConfigForReplay(ctx, dbsqlc.GetUserMachineInstallationConfigForReplayParams{VerifierHash: hash[:], PublicIdentityKey: publicIdentityKey, Now: now})
			if replayErr == nil {
				ciphertext = value
				return nil
			}
			if !errors.Is(replayErr, sql.ErrNoRows) {
				return replayErr
			}
			pairing, lookupErr := tx.Queries().GetUserMachinePairingForVerifier(ctx, hash[:])
			if errors.Is(lookupErr, sql.ErrNoRows) {
				return ErrInstallationUnavailable
			}
			if lookupErr != nil {
				return lookupErr
			}
			if pairing.State == "denied" {
				return ErrInstallationDenied
			}
			if pairing.State == "expired" {
				return ErrInstallationExpired
			}
			if pairing.State == "pending" {
				if !now.Before(pairing.ExpiresAt) {
					return ErrInstallationExpired
				}
				return ErrInstallationPending
			}
			if pairing.State == "consumed" && publicIdentityKey != "" && pairing.PublicIdentityKey.String == publicIdentityKey && pairing.InstallationConfigConsumedAt.Valid && pairing.InstallationConfigConsumedAt.Time.After(now.Add(-installationRecoveryGrace)) {
				switch pairing.InstallationState {
				case "material_issued", "installing", "connecting":
				case "ready":
					if !runtimeEnrolled {
						return ErrInstallationUnavailable
					}
				default:
					return ErrInstallationUnavailable
				}
				if !pairing.ApprovedByUserID.Valid {
					return ErrInstallationUnavailable
				}
				recoveryUserID = pairing.ApprovedByUserID.String
				recoveryOperationKey := pairing.InstallationRecoveryOperationKey.String
				if recoveryOperationKey == "" {
					recoveryOperationKey = installationRecoveryOperationKey(pairing.ID, pairing.ExpiresAt)
				}
				recoveryPairing, err = tx.Queries().BeginUserMachineInstallationRecovery(ctx, dbsqlc.BeginUserMachineInstallationRecoveryParams{
					ID: pairing.ID, VerifierHash: hash[:], PublicIdentityKey: sql.NullString{String: publicIdentityKey, Valid: true},
					RecoveryAfter: sql.NullTime{Time: now.Add(-installationRecoveryGrace), Valid: true}, OperationKey: sql.NullString{String: recoveryOperationKey, Valid: true},
					ExpiresAt: now.Add(s.policy.PairingLifetime),
				})
				if errors.Is(err, sql.ErrNoRows) {
					return ErrInstallationExpired
				}
				if err != nil {
					return err
				}
				{
					if n, err := tx.Queries().RenewPairingInstallationRecovery(ctx, dbsqlc.RenewPairingInstallationRecoveryParams{PairingID: pairing.ID, ExpiresAt: now.Add(s.policy.PairingLifetime), RuntimeEnrolled: runtimeEnrolled}); err != nil {
						return err
					} else if n != 1 {
						return ErrInstallationUnavailable
					}
				}
				return nil
			}
			if pairing.State == "consumed" && !now.Before(pairing.ExpiresAt) {
				return ErrInstallationExpired
			}
			if !now.Before(pairing.ExpiresAt) {
				return ErrInstallationExpired
			}
			return ErrInstallationUnavailable
		}
		if err != nil {
			return err
		}
		ciphertext = value
		return nil
	})
	if err != nil {
		return nil, err
	}
	if recoveryPairing.ID != "" {
		machineRow, machineErr := s.db.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: recoveryPairing.UserMachineID.String, UserID: recoveryUserID})
		if machineErr != nil {
			return nil, ErrInstallationUnavailable
		}
		material, err := s.provisionRecoveredUserMachine(ctx, recoveryUserID, recoveryPairing, mapMachine(machineRow), runtimeEnrolled)
		if err != nil {
			return nil, err
		}
		return material, nil
	}
	plaintext, err := secrets.Decrypt(s.encryptionKey, ciphertext)
	if err != nil || !json.Valid([]byte(plaintext)) {
		return nil, ErrInstallationUnavailable
	}
	return json.RawMessage(plaintext), nil
}

type Policy struct {
	PairingLifetime  time.Duration
	OfflineAfter     time.Duration
	AllowedPlatforms []string
	// ExpiredAccessSessionRetention is how long an expired access session may still be admitted.
	ExpiredAccessSessionRetention time.Duration
}
type Service struct {
	inventoryFlights            inventoryFlights
	runtimeQuota                accountingauthority.QuotaChecker
	runtimeBillingURL           string
	nameRandom                  io.Reader
	db                          *db.DB
	audit                       *audit.Writer
	policy                      Policy
	seats                       SeatAuthorizer
	now                         func() time.Time
	encryptionKey               string
	credentials                 access.CredentialIssuer
	issuer                      string
	fileTransferPolicy          accessdescriptor.FileTransferPolicy
	teamInbox                   *teaminbox.Service
	controlSigner               *mint.Provider
	controlRuntime              userMachineHelperRuntime
	bootstrapCommand            string
	helperGrant                 func(context.Context, string, string, string, time.Duration) (HelperEnrollmentGrant, error)
	helperRecovery              func(context.Context, string, string, string, string, time.Duration) (HelperEnrollmentGrant, error)
	authenticatedHelperGrant    func(context.Context, string, string, string, time.Duration, HelperEnrollmentAuthorityGuard) (HelperEnrollmentGrant, error)
	authenticatedHelperRecovery func(context.Context, string, string, string, string, time.Duration, HelperEnrollmentAuthorityGuard) (HelperEnrollmentGrant, error)
	helperBaseDomain            string
	helperListenPort            int32
	cliClientID                 string
	cliScopes                   string
	cliAccessLifetime           time.Duration
	cliRefreshLifetime          time.Duration
	cliHashKey                  []byte
}

type userMachineHelperRuntime interface {
	Terminal(context.Context, string, string, string, string, string) (helperruntime.Snapshot, error)
	TerminalViaEdge(context.Context, string, string, string, string, string, string) (helperruntime.Snapshot, error)
}

type HelperEnrollmentGrant struct {
	EnrollmentID string    `json:"enrollment_id"`
	HelperID     string    `json:"helper_id"`
	Credential   string    `json:"credential"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// HelperEnrollmentAuthorityGuard is called inside the transaction that makes
// authenticated Host helper authority durable. An empty enrollment ID locks
// and validates the exact pairing before issuance; the issued ID is then bound
// before that pairing lock is released. This serializes downgrade and session
// revocation with both grant persistence and disclosure.
type HelperEnrollmentAuthorityGuard func(context.Context, *db.Tx, string) error

// PersistAuthenticatedHelperEnrollment keeps the pairing locked across helper
// grant issuance and exact binding. Read committed is required because the
// enrollment service commits the grant in its own transaction; a serializable
// outer snapshot cannot see that new FK target. If binding cannot commit, the
// exact unbound pending grant is revoked before the error is returned.
func PersistAuthenticatedHelperEnrollment(ctx context.Context, store *db.DB, environmentID string, guard HelperEnrollmentAuthorityGuard, issue func(context.Context) (HelperEnrollmentGrant, error)) (HelperEnrollmentGrant, error) {
	if store == nil || strings.TrimSpace(environmentID) == "" || guard == nil || issue == nil {
		return HelperEnrollmentGrant{}, ErrProvisioningUnavailable
	}
	var result HelperEnrollmentGrant
	err := store.InReadCommittedTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := guard(ctx, tx, ""); err != nil {
			return err
		}
		grant, err := issue(ctx)
		if err != nil {
			return err
		}
		if strings.TrimSpace(grant.EnrollmentID) == "" {
			return ErrProvisioningUnavailable
		}
		result = grant
		return guard(ctx, tx, grant.EnrollmentID)
	})
	if err == nil {
		return result, nil
	}
	if strings.TrimSpace(result.EnrollmentID) == "" {
		return HelperEnrollmentGrant{}, err
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var safe bool
	cleanupErr := store.InReadCommittedTx(cleanupCtx, func(ctx context.Context, tx *db.Tx) error {
		// The empty guard acquires the same pairing lock as bind/revocation.
		// Its validation result is intentionally ignored here: an expired or
		// revoked pairing still requires cleanup of any unbound pending grant.
		_ = guard(ctx, tx, "")
		var cleanupErr error
		safe, cleanupErr = tx.Queries().RevokeUnboundAuthenticatedHostHelperEnrollment(ctx, dbsqlc.RevokeUnboundAuthenticatedHostHelperEnrollmentParams{
			HelperEnrollmentID: result.EnrollmentID,
			Now:                time.Now().UTC(),
			EnvironmentID:      environmentID,
		})
		return cleanupErr
	})
	if cleanupErr == nil && !safe {
		cleanupErr = errors.New("helper enrollment remained consumed without an active authenticated Host binding")
	}
	if cleanupErr != nil {
		err = errors.Join(err, fmt.Errorf("revoke unbound authenticated Host helper enrollment: %w", cleanupErr))
	}
	return HelperEnrollmentGrant{}, err
}

func (s *Service) FailInstallation(ctx context.Context, enrollmentID, environmentID, helperID, helperEnrollmentID, stage string) error {
	if strings.TrimSpace(enrollmentID) == "" || strings.TrimSpace(environmentID) == "" || strings.TrimSpace(helperID) == "" || strings.TrimSpace(helperEnrollmentID) == "" || !slices.Contains([]string{"artifact_verification", "service_install", "service_readiness"}, stage) {
		return ErrEnrollmentState
	}
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		result, err := tx.Queries().FailPairingInstallationForHelper(ctx, dbsqlc.FailPairingInstallationForHelperParams{ID: enrollmentID, EnvironmentID: environmentID, HelperID: helperID, HelperEnrollmentID: helperEnrollmentID})
		if err != nil {
			return err
		}
		if result.FailedCount != 1 || result.ReleasedCount != 1 || result.RevokedHelperCount != 1 || result.RevokedGrantCount < 1 {
			return ErrEnrollmentState
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorType: audit.ActorSystem, EventType: "user_machine.installation_failed", ResourceType: "pairing", ResourceID: enrollmentID, IdempotencyKey: "user_machine.installation_failed:" + enrollmentID + ":" + stage, Metadata: map[string]any{"environment_id": environmentID, "helper_id": helperID, "stage": stage}})
	})
}

// Worker retries revocations after the connector becomes reachable again. A
// machine may be offline when a user disconnects it, so revocation must not
// depend on a synchronous Helper response.
func (s *Service) Worker(interval time.Duration) func(context.Context) error {
	if interval <= 0 {
		interval = time.Second
	}
	return func(ctx context.Context) error {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			finishProgress := workers.BeginIteration(ctx, interval)
			cycleErr := func() error {
				workers.IterationFailure(ctx, "pending_revocation", "machine_revocation_failed", s.RetryPendingRevocations(ctx))
				workers.IterationFailure(ctx, "terminal_session_operation", "terminal_operation_failed", s.processDueTerminalSessionOperations(ctx))
				workers.IterationFailure(ctx, "terminal_session_cleanup", "terminal_cleanup_failed", s.expireClosedTerminalSessions(ctx))
				workers.IterationFailure(ctx, "terminal_session_orphans", "terminal_orphan_reconcile_failed", s.reconcileOrphanTerminalSessions(ctx))
				_, progressErr := s.db.Queries().MarkStaleUserMachinesOffline(ctx, sql.NullTime{Time: s.now().UTC().Add(-s.policy.OfflineAfter), Valid: true})
				workers.IterationFailure(ctx, "stale_machine_offline", "machine_offline_reconcile_failed", progressErr)
				workers.IterationFailure(ctx, "access_session_retention", "access_session_retention_failed", s.retireExpiredAccessSessions(ctx))

				return nil
			}()
			finishProgress(cycleErr)
			if cycleErr != nil {
				return cycleErr
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	}
}

// retireExpiredAccessSessions stops expired access sessions from staying active (and from being
// named as admitted) forever.
func (s *Service) retireExpiredAccessSessions(ctx context.Context) error {
	if s.policy.ExpiredAccessSessionRetention <= 0 {
		return nil
	}
	_, err := s.db.Queries().RetireExpiredMachineAccessSessions(ctx, s.now().UTC().Add(-s.policy.ExpiredAccessSessionRetention))
	return err
}

func (s *Service) ConfigureInstallationEncryption(encryptionKey string) {
	s.encryptionKey = encryptionKey
}

func (s *Service) ConfigureAccess(credentials access.CredentialIssuer, issuer string) {
	s.credentials, s.issuer = credentials, strings.TrimRight(issuer, "/")
}

// ConfigureCLIAuth supplies the same CLI credential policy used by browser login.
// Approved installation material carries an ordinary CLI session.
func (s *Service) ConfigureCLIAuth(clientID string, scopes []string, accessLifetime, refreshLifetime time.Duration, hashKey string) {
	s.cliClientID = strings.TrimSpace(clientID)
	s.cliScopes = strings.Join(slices.Compact(slices.Clone(scopes)), " ")
	s.cliAccessLifetime, s.cliRefreshLifetime = accessLifetime, refreshLifetime
	s.cliHashKey = []byte(hashKey)
}

func (s *Service) ConfigureFileTransfer(policy accessdescriptor.FileTransferPolicy) {
	s.fileTransferPolicy = policy
}

func (s *Service) ConfigureTeamInbox(service *teaminbox.Service) { s.teamInbox = service }

// RuntimeFileTransferPolicy exposes the configured policy on authenticated runtime heartbeats.
func (s *Service) RuntimeFileTransferPolicy() accessdescriptor.FileTransferPolicy {
	return s.fileTransferPolicy
}

func (s *Service) ConfigureTerminalSessions(signer *mint.Provider, client *http.Client) {
	s.controlSigner = signer
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	s.controlRuntime = helperruntime.Client{HTTPClient: client}
}

func (s *Service) ConfigureBootstrapCommand(command string) {
	s.bootstrapCommand = strings.TrimSpace(command)
}

func (s *Service) ConfigureRuntimeRoute(baseDomain string, listenPort int32) error {
	baseDomain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(baseDomain), "."))
	if baseDomain == "" || listenPort < 1024 || listenPort > 65535 {
		return errors.New("machine runtime route configuration is invalid")
	}
	s.helperBaseDomain, s.helperListenPort = baseDomain, listenPort
	return nil
}

func (s *Service) ConfigureHelperEnrollment(issuer func(context.Context, string, string, string, time.Duration) (HelperEnrollmentGrant, error)) {
	s.helperGrant = issuer
}

func (s *Service) ConfigureHelperRecovery(recover func(context.Context, string, string, string, string, time.Duration) (HelperEnrollmentGrant, error)) {
	s.helperRecovery = recover
}

func (s *Service) ConfigureAuthenticatedHelperEnrollment(
	issuer func(context.Context, string, string, string, time.Duration, HelperEnrollmentAuthorityGuard) (HelperEnrollmentGrant, error),
	recover func(context.Context, string, string, string, string, time.Duration, HelperEnrollmentAuthorityGuard) (HelperEnrollmentGrant, error),
) {
	s.authenticatedHelperGrant, s.authenticatedHelperRecovery = issuer, recover
}

func New(store *db.DB, auditWriter *audit.Writer, policy Policy, seats SeatAuthorizer) *Service {
	if policy.OfflineAfter <= 0 {
		policy.OfflineAfter = 2 * time.Minute
	}
	return &Service{db: store, audit: auditWriter, policy: policy, seats: seats, now: time.Now}
}

type PairingInput struct {
	Verifier, EnrollmentToken, Alias, Description, Platform, Architecture, WorkspaceRoot, PublicIdentityKey string
	SSHUser                                                                                                 string
	SSHPort                                                                                                 uint16
	RuntimeVersions                                                                                         json.RawMessage
	AcceptBetaPlatform                                                                                      bool
	CanReuseRuntimeIdentity                                                                                 bool
	KeepAwake                                                                                               bool
	// ReplacesMachineID names the record of the computer this install starts over from. It is a
	// request: approval only acts on a retired record of the same account.
	ReplacesMachineID string
}

type Enrollment struct {
	ID                   string     `json:"id"`
	OperationID          string     `json:"operation_id"`
	State                string     `json:"state"`
	Generation           int64      `json:"generation"`
	UserCode             string     `json:"user_code,omitempty"`
	UserMachineID        string     `json:"machine_id,omitempty"`
	RequestedAlias       string     `json:"requested_alias,omitempty"`
	RequestedDescription string     `json:"requested_description,omitempty"`
	Platform             string     `json:"platform,omitempty"`
	Architecture         string     `json:"architecture,omitempty"`
	WorkspaceRoot        string     `json:"workspace_root,omitempty"`
	ExpiresAt            time.Time  `json:"expires_at"`
	CancelledAt          *time.Time `json:"cancelled_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

type EnrollmentStart struct {
	Enrollment
	// BootstrapToken is a short-lived, single-use credential. It is returned
	// only when an enrollment is created so the dashboard can render one
	// paste-once command. It is never persisted in plaintext or logged.
	BootstrapToken   string `json:"bootstrap_token"`
	BootstrapCommand string `json:"bootstrap_command"`
	ServerURL        string `json:"server_url"`
}

type EnrollmentOptions struct {
	Shell string // posix or powershell
	// WorkspaceTeamID is the team the machine joins when it enrolls; empty keeps it Personal.
	WorkspaceTeamID string
}

func (s *Service) StartEnrollment(ctx context.Context, userID, idempotencyKey string) (EnrollmentStart, error) {
	return s.StartEnrollmentWithOptions(ctx, userID, idempotencyKey, EnrollmentOptions{Shell: "posix"})
}

func (s *Service) StartEnrollmentWithOptions(ctx context.Context, userID, idempotencyKey string, options EnrollmentOptions) (EnrollmentStart, error) {
	userID, idempotencyKey = strings.TrimSpace(userID), strings.TrimSpace(idempotencyKey)
	if userID == "" || idempotencyKey == "" || len(idempotencyKey) > 128 {
		return EnrollmentStart{}, ErrIdempotencyKeyRequired
	}
	if team := options.WorkspaceTeamID; team != "" {
		if err := workspace.Validate(ctx, s.db, userID, workspace.Scope{ID: team, TeamID: team}); err != nil {
			if errors.Is(err, workspace.ErrForbidden) || errors.Is(err, workspace.ErrInvalid) {
				return EnrollmentStart{}, ErrWorkspaceForbidden
			}
			return EnrollmentStart{}, err
		}
	}
	if strings.TrimSpace(s.encryptionKey) == "" {
		return EnrollmentStart{}, errors.New("user-machine enrollment encryption is not configured")
	}
	token, err := randomEnrollmentTokenFor(options.Shell)
	if err != nil {
		return EnrollmentStart{}, err
	}
	hash := enrollmentTokenHash(token)
	ciphertext, err := secrets.Encrypt(s.encryptionKey, token)
	if err != nil {
		return EnrollmentStart{}, err
	}
	expires := s.now().UTC().Add(s.policy.PairingLifetime)
	row, err := s.db.Queries().CreateBootstrapPairing(ctx, dbsqlc.CreateBootstrapPairingParams{
		ID: newID("pairing"), UserID: sql.NullString{String: userID, Valid: true}, OperationID: sql.NullString{String: newID("operation"), Valid: true}, IdempotencyKey: sql.NullString{String: idempotencyKey, Valid: true},
		BootstrapTokenHash: hash[:], BootstrapTokenCiphertext: ciphertext, ExpiresAt: expires,
		WorkspaceTeamID: sql.NullString{String: options.WorkspaceTeamID, Valid: options.WorkspaceTeamID != ""},
	})
	if err != nil {
		return EnrollmentStart{}, err
	}
	if !bytes.Equal(row.BootstrapTokenHash, hash[:]) {
		token, err = secrets.Decrypt(s.encryptionKey, row.BootstrapTokenCiphertext)
		if err != nil {
			return EnrollmentStart{}, err
		}
	}
	result := s.enrollmentStart(row, token)
	_ = recordEnrollmentStartedAudit(ctx, s.audit, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "user_machine.enrollment_started", ResourceType: "pairing", ResourceID: row.ID, IdempotencyKey: "user_machine.enrollment_started:" + row.ID, Metadata: map[string]any{"operation_id": row.OperationID, "generation": row.Generation}})
	return result, nil
}

// recordEnrollmentStartedAudit is best-effort because the pairing is already
// committed. A failed audit write is reported once without changing the
// successful enrollment response.
func recordEnrollmentStartedAudit(ctx context.Context, writer *audit.Writer, event audit.Event) error {
	err := writer.Write(ctx, event)
	if err != nil {
		supportreport.CaptureFromContext(ctx, "paperboat-server", "http_machines", "service_error", "user_machine_enrollment_audit_failed", err)
	}
	return err
}

func (s *Service) Enrollment(ctx context.Context, userID, enrollmentID string) (Enrollment, error) {
	if _, err := s.db.Queries().ExpireBootstrapPairing(ctx, dbsqlc.ExpireBootstrapPairingParams{ID: strings.TrimSpace(enrollmentID), UserID: sql.NullString{String: strings.TrimSpace(userID), Valid: true}}); err != nil {
		return Enrollment{}, err
	}
	row, err := s.db.Queries().GetBootstrapPairingForUser(ctx, dbsqlc.GetBootstrapPairingForUserParams{ID: strings.TrimSpace(enrollmentID), UserID: sql.NullString{String: strings.TrimSpace(userID), Valid: true}})
	if errors.Is(err, sql.ErrNoRows) {
		return Enrollment{}, ErrEnrollmentNotFound
	}
	if err != nil {
		return Enrollment{}, err
	}
	result := mapEnrollment(row)
	result.UserCode = row.UserCode.String
	return result, nil
}

func (s *Service) CancelEnrollment(ctx context.Context, userID, enrollmentID string) error {
	n, err := s.db.Queries().CancelBootstrapPairing(ctx, dbsqlc.CancelBootstrapPairingParams{ID: strings.TrimSpace(enrollmentID), UserID: sql.NullString{String: strings.TrimSpace(userID), Valid: true}})
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrEnrollmentState
	}
	return s.audit.Write(ctx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "user_machine.enrollment_cancelled", ResourceType: "pairing", ResourceID: enrollmentID, IdempotencyKey: "user_machine.enrollment_cancelled:" + enrollmentID})
}

func (s *Service) RetryEnrollment(ctx context.Context, userID, enrollmentID string) (EnrollmentStart, error) {
	return s.RetryEnrollmentWithOptions(ctx, userID, enrollmentID, EnrollmentOptions{Shell: "posix"})
}

func (s *Service) RetryEnrollmentWithOptions(ctx context.Context, userID, enrollmentID string, options EnrollmentOptions) (EnrollmentStart, error) {
	if strings.TrimSpace(s.encryptionKey) == "" {
		return EnrollmentStart{}, errors.New("user-machine enrollment encryption is not configured")
	}
	token, err := randomEnrollmentTokenFor(options.Shell)
	if err != nil {
		return EnrollmentStart{}, err
	}
	hash := enrollmentTokenHash(token)
	ciphertext, err := secrets.Encrypt(s.encryptionKey, token)
	if err != nil {
		return EnrollmentStart{}, err
	}
	row, err := s.db.Queries().RetryBootstrapPairing(ctx, dbsqlc.RetryBootstrapPairingParams{BootstrapTokenHash: hash[:], BootstrapTokenCiphertext: ciphertext, ExpiresAt: s.now().UTC().Add(s.policy.PairingLifetime), ID: strings.TrimSpace(enrollmentID), UserID: sql.NullString{String: strings.TrimSpace(userID), Valid: true}})
	if errors.Is(err, sql.ErrNoRows) {
		return EnrollmentStart{}, ErrEnrollmentState
	}
	if err != nil {
		return EnrollmentStart{}, err
	}
	return s.enrollmentStart(row, token), nil
}

func (s *Service) enrollmentStart(row dbsqlc.UserMachinePairing, token string) EnrollmentStart {
	return EnrollmentStart{
		Enrollment:       mapEnrollment(row),
		BootstrapToken:   token,
		BootstrapCommand: strings.TrimSpace(s.bootstrapCommand),
		ServerURL:        strings.TrimRight(strings.TrimSpace(s.issuer), "/"),
	}
}

func mapEnrollment(row dbsqlc.UserMachinePairing) Enrollment {
	result := Enrollment{ID: row.ID, OperationID: row.OperationID.String, State: row.InstallationState, Generation: row.Generation, UserMachineID: row.UserMachineID.String, RequestedAlias: row.RequestedAlias.String, RequestedDescription: row.RequestedDescription, Platform: row.Platform.String, Architecture: row.Architecture.String, WorkspaceRoot: row.WorkspaceRoot.String, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	if row.CancelledAt.Valid {
		value := row.CancelledAt.Time
		result.CancelledAt = &value
	}
	return result
}

type Pairing struct {
	ID        string    `json:"id"`
	UserCode  string    `json:"user_code"`
	ExpiresAt time.Time `json:"expires_at"`
}

type AuthenticatedMachineInstallationInput struct {
	OperationID, Verifier, PublicIdentityKey string
	InstallationGeneration                   int64
	SSHUser                                  string
	SSHPort                                  uint16
	CanReuseRuntimeIdentity                  bool
	KeepAwake                                bool
}

type AuthenticatedMachineInstallation struct {
	ExpiresAt time.Time `json:"expires_at"`
}

// PrepareAuthenticatedMachineInstallation issues verifier-bound installation material
// only after an authenticated CLI proves the exact machine enrollment.
// The CLI session and operation ID make retries idempotent, while the machine
// identity, generation, and installation readiness prevent authority from crossing setup
// attempts.
func (s *Service) PrepareAuthenticatedMachineInstallation(ctx context.Context, userID, cliClientSessionID, machineID string, in AuthenticatedMachineInstallationInput) (AuthenticatedMachineInstallation, error) {
	userID, cliClientSessionID, machineID = strings.TrimSpace(userID), strings.TrimSpace(cliClientSessionID), strings.TrimSpace(machineID)
	in.OperationID, in.Verifier, in.PublicIdentityKey = strings.TrimSpace(in.OperationID), strings.TrimSpace(in.Verifier), strings.TrimSpace(in.PublicIdentityKey)
	if userID == "" || cliClientSessionID == "" || machineID == "" || !execOperationIDPattern.MatchString(in.OperationID) || len(in.Verifier) < 32 || len(in.Verifier) > 256 || in.InstallationGeneration < 1 {
		return AuthenticatedMachineInstallation{}, ErrInvalidMachineInstallation
	}
	verifierHash := sha256.Sum256([]byte(in.Verifier))
	userCode, err := randomCode(8)
	if err != nil {
		return AuthenticatedMachineInstallation{}, err
	}
	expiresAt := s.now().UTC().Add(s.policy.PairingLifetime)
	var machine UserMachine
	var pairing dbsqlc.UserMachinePairing
	var recoveredExistingPairing bool
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var loadErr error
		machine, loadErr = s.authenticatedMachineInstallationMachineForUpdate(ctx, tx, userID, cliClientSessionID, machineID, in)
		if loadErr != nil {
			return loadErr
		}
		now := s.now().UTC()
		existing, existingErr := tx.Queries().GetUserMachinePairingForVerifier(ctx, verifierHash[:])
		if existingErr == nil {
			if !authenticatedMachineInstallationPairingFieldsMatch(existing, userID, machine, in, verifierHash[:]) {
				return ErrMachineInstallationOperationConflict
			}
			if len(existing.InstallationConfigCiphertext) == 0 {
				if existing.State != "approved" || !existing.AuthenticatedSetupCLISessionID.Valid || existing.AuthenticatedSetupCLISessionID.String != cliClientSessionID || !now.Before(existing.ExpiresAt) {
					return ErrMachineInstallationOperationConflict
				}
				pairing = existing
				return nil
			}
			if len(existing.InstallationConfigCiphertext) > 0 {
				materialSessionID, materialErr := s.authenticatedMachineInstallationMaterialForRetry(existing, machine, in)
				if materialErr != nil {
					return materialErr
				}
				if callerErr := s.authorizeAuthenticatedMachineInstallationRetry(ctx, tx, userID, cliClientSessionID, machine, existing, materialSessionID); callerErr != nil {
					return callerErr
				}
				switch existing.State {
				case "approved":
					if !now.Before(existing.ExpiresAt) || existing.InstallationConfigConsumedAt.Valid {
						return ErrMachineInstallationOperationConflict
					}
					pairing = existing
					recoveredExistingPairing = true
				case "consumed":
					if now.Before(existing.ExpiresAt) {
						pairing = existing
						recoveredExistingPairing = true
						break
					}
					fallthrough
				case "expired":
					if !existing.InstallationConfigConsumedAt.Valid || !existing.InstallationConfigConsumedAt.Time.After(now.Add(-installationRecoveryGrace)) {
						return ErrMachineInstallationOperationConflict
					}
					recoveryOperationKey := existing.InstallationRecoveryOperationKey.String
					if recoveryOperationKey == "" {
						recoveryOperationKey = installationRecoveryOperationKey(existing.ID, existing.ExpiresAt)
					}
					pairing, loadErr = tx.Queries().BeginUserMachineInstallationRecovery(ctx, dbsqlc.BeginUserMachineInstallationRecoveryParams{
						ID: existing.ID, VerifierHash: verifierHash[:], PublicIdentityKey: sql.NullString{String: machine.PublicIdentityKey, Valid: true},
						OperationKey:  sql.NullString{String: recoveryOperationKey, Valid: true},
						RecoveryAfter: sql.NullTime{Time: now.Add(-installationRecoveryGrace), Valid: true},
						ExpiresAt:     now.Add(s.policy.PairingLifetime),
					})
					if errors.Is(loadErr, sql.ErrNoRows) {
						return ErrMachineInstallationOperationConflict
					}
					if loadErr != nil {
						return loadErr
					}
					recoveredExistingPairing = true
				default:
					return ErrMachineInstallationOperationConflict
				}
				return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "machine.machine_installation_prepared", ResourceType: "machine", ResourceID: machine.ID, IdempotencyKey: "machine.machine_installation_prepared:" + cliClientSessionID + ":" + in.OperationID, Metadata: map[string]any{"installation_generation": in.InstallationGeneration}})
			}
		}
		if !errors.Is(existingErr, sql.ErrNoRows) {
			return existingErr
		}
		pairing, loadErr = tx.Queries().CreateAuthenticatedMachineInstallationPairing(ctx, dbsqlc.CreateAuthenticatedMachineInstallationPairingParams{
			ID:           newID("pairing"),
			VerifierHash: verifierHash[:], UserCode: sql.NullString{String: userCode, Valid: true},
			RequestedAlias: sql.NullString{String: machine.Alias, Valid: true}, RequestedDescription: machine.Description, Platform: sql.NullString{String: machine.Platform, Valid: true}, Architecture: sql.NullString{String: machine.Architecture, Valid: true},
			WorkspaceRoot: sql.NullString{String: machine.WorkspaceRoot, Valid: true}, RuntimeVersions: machine.RuntimeVersions,
			PublicIdentityKey:       sql.NullString{String: machine.PublicIdentityKey, Valid: true},
			SshUser:                 sql.NullString{String: in.SSHUser, Valid: in.SSHUser != ""},
			SshPort:                 sql.NullInt32{Int32: int32(in.SSHPort), Valid: in.SSHPort != 0},
			CanReuseRuntimeIdentity: in.CanReuseRuntimeIdentity,
			KeepAwake:               in.KeepAwake,
			UserID:                  sql.NullString{String: userID, Valid: true},
			UserMachineID:           sql.NullString{String: machine.ID, Valid: true},
			CLIClientSessionID:      sql.NullString{String: cliClientSessionID, Valid: true},
			OperationID:             sql.NullString{String: in.OperationID, Valid: true},
			InstallationGeneration:  sql.NullInt64{Int64: in.InstallationGeneration, Valid: true},
			ExpiresAt:               expiresAt,
		})
		if loadErr != nil {
			if userMachineTerminalSessionUniqueViolation(loadErr) {
				return ErrMachineInstallationOperationConflict
			}
			return loadErr
		}
		if !authenticatedMachineInstallationPairingMatches(pairing, userID, cliClientSessionID, machine, in, verifierHash[:], s.now().UTC()) {
			return ErrMachineInstallationOperationConflict
		}
		if pairing.KeepAwake && machine.Availability.DesiredMode != "keep_awake" {
			if _, loadErr = tx.Queries().OptInUserMachineKeepAwake(ctx, dbsqlc.OptInUserMachineKeepAwakeParams{ID: machine.ID, UserID: userID}); loadErr != nil {
				return loadErr
			}
		}
		return nil
	})
	if err != nil {
		return AuthenticatedMachineInstallation{}, err
	}
	if recoveredExistingPairing {
		return AuthenticatedMachineInstallation{ExpiresAt: pairing.ExpiresAt}, nil
	}

	var candidateBuild installationMaterialBuild
	var candidateCiphertext []byte
	if len(pairing.InstallationConfigCiphertext) == 0 {
		operationKey := "authenticated-machine-installation:" + pairing.ID
		authorityGuard := func(ctx context.Context, tx *db.Tx, helperEnrollmentID string) error {
			_, _, guardErr := s.authenticatedMachineInstallationForPairingLock(ctx, tx, userID, cliClientSessionID, machineID, pairing.ID, in, verifierHash[:], "approved")
			if guardErr != nil || helperEnrollmentID == "" {
				return guardErr
			}
			if n, bindErr := tx.Queries().BindAuthenticatedMachineInstallationHelperEnrollment(ctx, dbsqlc.BindAuthenticatedMachineInstallationHelperEnrollmentParams{ID: pairing.ID, HelperEnrollmentID: sql.NullString{String: helperEnrollmentID, Valid: true}}); bindErr != nil {
				return bindErr
			} else if n != 1 {
				return ErrMachineInstallationOperationConflict
			}
			return nil
		}
		build, buildErr := s.buildInstallationMaterial(ctx, userID, pairing, machine, operationKey, nil, true, in.CanReuseRuntimeIdentity, "", authorityGuard)
		if buildErr != nil {
			return AuthenticatedMachineInstallation{}, buildErr
		}
		candidateBuild = build
		candidateCiphertext = build.ciphertext
	}
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		currentMachine, current, loadErr := s.authenticatedMachineInstallationForPairingLock(ctx, tx, userID, cliClientSessionID, machineID, pairing.ID, in, verifierHash[:], "approved")
		if loadErr != nil {
			return loadErr
		}
		if len(current.InstallationConfigCiphertext) == 0 {
			if len(candidateCiphertext) == 0 {
				return ErrMachineInstallationOperationConflict
			}
			if err := s.persistInstallationCLISessionTx(ctx, tx, userID, currentMachine, candidateBuild); err != nil {
				return err
			}
			if n, setErr := tx.Queries().SetUserMachineInstallationConfig(ctx, dbsqlc.SetUserMachineInstallationConfigParams{ID: current.ID, Ciphertext: candidateCiphertext}); setErr != nil {
				return setErr
			} else if n != 1 {
				return ErrMachineInstallationOperationConflict
			}
			current.InstallationConfigCiphertext = candidateCiphertext
		}
		if !s.authenticatedMachineInstallationMaterialMatches(current.InstallationConfigCiphertext, currentMachine.ID, currentMachine.EnvironmentID, in.InstallationGeneration, current.AuthenticatedSetupHelperEnrollmentID) {
			return ErrMachineInstallationOperationConflict
		}
		pairing = current
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "machine.machine_installation_prepared", ResourceType: "machine", ResourceID: currentMachine.ID, IdempotencyKey: "machine.machine_installation_prepared:" + cliClientSessionID + ":" + in.OperationID, Metadata: map[string]any{"installation_generation": in.InstallationGeneration}})
	})
	if err != nil {
		return AuthenticatedMachineInstallation{}, err
	}
	return AuthenticatedMachineInstallation{ExpiresAt: pairing.ExpiresAt}, nil
}

func (s *Service) authenticatedMachineInstallationMachineForUpdate(ctx context.Context, tx *db.Tx, userID, cliClientSessionID, machineID string, in AuthenticatedMachineInstallationInput) (UserMachine, error) {
	session, err := tx.Queries().GetClientSessionForInstallationRecovery(ctx, cliClientSessionID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && session.UserID != userID {
		return UserMachine{}, ErrInvalidMachineInstallation
	}
	if err != nil {
		return UserMachine{}, err
	}
	row, err := tx.Queries().GetUserMachineForUpdate(ctx, dbsqlc.GetUserMachineForUpdateParams{ID: machineID, UserID: userID})
	if errors.Is(err, sql.ErrNoRows) {
		return UserMachine{}, ErrInvalidMachineInstallation
	}
	if err != nil {
		return UserMachine{}, err
	}
	return s.validateAuthenticatedMachineInstallationMachine(mapMachine(row), in)
}

// authenticatedMachineInstallationForPairingLock uses the pairing as the common
// serialization row for issuance, host-authority revocation, entitlement loss,
// machine removal, and CLI-session revocation. Session and machine reads stay
// non-locking after that row is held, so a transition that already owns the
// machine or session row can wait for issuance and then revoke its grant
// without forming a lock cycle.
func (s *Service) authenticatedMachineInstallationForPairingLock(ctx context.Context, tx *db.Tx, userID, cliClientSessionID, machineID, pairingID string, in AuthenticatedMachineInstallationInput, verifierHash []byte, requiredState string) (UserMachine, dbsqlc.UserMachinePairing, error) {
	pairing, err := tx.Queries().GetUserMachinePairingForVerifier(ctx, verifierHash)
	if err != nil || pairing.ID != pairingID || pairing.State != requiredState {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return UserMachine{}, dbsqlc.UserMachinePairing{}, err
		}
		return UserMachine{}, dbsqlc.UserMachinePairing{}, ErrMachineInstallationOperationConflict
	}
	session, err := tx.Queries().GetActiveClientSessionForInstallation(ctx, cliClientSessionID)
	if err != nil || session.UserID != userID {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return UserMachine{}, dbsqlc.UserMachinePairing{}, err
		}
		return UserMachine{}, dbsqlc.UserMachinePairing{}, ErrInvalidMachineInstallation
	}
	row, err := tx.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: machineID, UserID: userID})
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return UserMachine{}, dbsqlc.UserMachinePairing{}, err
		}
		return UserMachine{}, dbsqlc.UserMachinePairing{}, ErrInvalidMachineInstallation
	}
	machine, err := s.validateAuthenticatedMachineInstallationMachine(mapMachine(row), in)
	if err != nil {
		return UserMachine{}, dbsqlc.UserMachinePairing{}, err
	}
	if !authenticatedMachineInstallationPairingMatches(pairing, userID, cliClientSessionID, machine, in, verifierHash, s.now().UTC()) {
		return UserMachine{}, dbsqlc.UserMachinePairing{}, ErrMachineInstallationOperationConflict
	}
	return machine, pairing, nil
}
