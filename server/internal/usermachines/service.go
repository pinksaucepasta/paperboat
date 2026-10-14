package usermachines

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat-server/internal/workers"

	"github.com/pinksaucepasta/paperboat-server/internal/access"
	"github.com/pinksaucepasta/paperboat-server/internal/accessdescriptor"
	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/environment"
	"github.com/pinksaucepasta/paperboat-server/internal/helperruntime"
	"github.com/pinksaucepasta/paperboat-server/internal/machinealias"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
	"github.com/pinksaucepasta/paperboat-server/internal/naming"
	"github.com/pinksaucepasta/paperboat-server/internal/releases"
	"github.com/pinksaucepasta/paperboat-server/internal/secrets"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
	"github.com/pinksaucepasta/paperboat-server/internal/teaminbox"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
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
	ErrEnrollmentState                      = errors.New("user-machine enrollment state does not allow this operation")
	ErrIdempotencyKeyRequired               = errors.New("user-machine enrollment idempotency key is required")
	ErrTerminalSessionNotFound              = errors.New("user-machine terminal session not found")
	ErrTerminalSessionReserved              = errors.New("user-machine default terminal session is reserved")
	ErrTerminalSessionLimit                 = errors.New("user-machine terminal session limit reached")
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

// TerminalSessionCapacityError reports why a new terminal cannot be reserved
// on a machine while preserving ErrTerminalSessionLimit for existing callers.
type TerminalSessionCapacityError struct {
	LiveSessions int
	Limit        int
}

func (e *TerminalSessionCapacityError) Error() string {
	return fmt.Sprintf("This machine has %d running terminals (limit %d). Close one to start another.", e.LiveSessions, e.Limit)
}

func (e *TerminalSessionCapacityError) Unwrap() error { return ErrTerminalSessionLimit }

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

func (s *Service) consumeInstallation(ctx context.Context, verifier, publicIdentityKey string, runtimeEnrolled bool) (json.RawMessage, error) {
	if strings.TrimSpace(verifier) == "" || strings.TrimSpace(s.encryptionKey) == "" {
		return nil, ErrInstallationUnavailable
	}
	hash := sha256.Sum256([]byte(verifier))
	now := s.now().UTC()
	var ciphertext []byte
	var recoveryPairing dbsqlc.UserMachinePairing
	var recoveryUserID string
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
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
					if n, err := tx.Queries().RenewPairingInstallationRecovery(ctx, dbsqlc.RenewPairingInstallationRecoveryParams{PairingID: pairing.ID, ExpiresAt: now.Add(s.policy.PairingLifetime)}); err != nil {
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
}
type Service struct {
	nameRandom                  io.Reader
	db                          *db.DB
	audit                       *audit.Writer
	policy                      Policy
	seats                       SeatAuthorizer
	now                         func() time.Time
	encryptionKey               string
	credentials                 access.CredentialIssuer
	issuer                      string
	ttl                         time.Duration
	fileTransferPolicy          accessdescriptor.FileTransferPolicy
	teamInbox                   *teaminbox.Service
	maxSessions                 int
	controlSigner               *mint.Provider
	controlRuntime              userMachineHelperRuntime
	bootstrapCommand            string
	helperGrant                 func(context.Context, string, string, string, time.Duration) (HelperEnrollmentGrant, error)
	helperRecovery              func(context.Context, string, string, string, string, time.Duration) (HelperEnrollmentGrant, error)
	authenticatedHelperGrant    func(context.Context, string, string, string, time.Duration, HelperEnrollmentAuthorityGuard) (HelperEnrollmentGrant, error)
	authenticatedHelperRecovery func(context.Context, string, string, string, string, time.Duration, HelperEnrollmentAuthorityGuard) (HelperEnrollmentGrant, error)
	artifactRepository          string
	artifactVersion             string
	artifactVersionFn           func(string, string) string
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

type MachineArtifact struct {
	Schema        string `json:"schema"`
	Kind          string `json:"kind"`
	Version       string `json:"version"`
	Platform      string `json:"platform"`
	Architecture  string `json:"architecture"`
	RepositoryURL string `json:"repository_url"`
	TargetPath    string `json:"target_path"`
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
				_, progressErr := s.db.Queries().MarkStaleUserMachinesOffline(ctx, sql.NullTime{Time: s.now().UTC().Add(-s.policy.OfflineAfter), Valid: true})
				workers.IterationFailure(ctx, "stale_machine_offline", "machine_offline_reconcile_failed", progressErr)

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

func (s *Service) ConfigureInstallationEncryption(encryptionKey string) {
	s.encryptionKey = encryptionKey
}

func (s *Service) ConfigureAccess(credentials access.CredentialIssuer, issuer string, ttl time.Duration) {
	s.credentials, s.issuer, s.ttl = credentials, strings.TrimRight(issuer, "/"), ttl
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

func (s *Service) ConfigureTerminalSessions(maxActive int, signer *mint.Provider, client *http.Client) {
	if maxActive > 0 {
		s.maxSessions = min(maxActive, 20)
	}
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

func (s *Service) ConfigureMachineArtifacts(repositoryURL, version string) error {
	repositoryURL, version = strings.TrimRight(strings.TrimSpace(repositoryURL), "/"), strings.TrimSpace(version)
	if repositoryURL == "" && version == "" {
		return nil
	}
	parsed, err := url.Parse(repositoryURL)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" || parsed.RawQuery != "" || parsed.Fragment != "" || version == "" || len(version) > 64 || strings.ContainsAny(version, "\x00\r\n/\\") {
		return errors.New("user-machine artifacts are invalid")
	}
	s.artifactRepository, s.artifactVersion = repositoryURL, version
	return nil
}

func (s *Service) ConfigureMachineArtifactVersionResolver(resolve func(string, string) string) {
	s.artifactVersionFn = resolve
}

func (s *Service) machineArtifact(platform, architecture string) (MachineArtifact, bool) {
	version := s.artifactVersion
	if s.artifactVersionFn != nil {
		version = strings.TrimSpace(s.artifactVersionFn(platform, architecture))
	}
	if s.artifactRepository == "" || version == "" || len(version) > 64 || strings.ContainsAny(version, "\x00\r\n/\\") || !releases.SupportedPlatformArchitecture(platform, architecture) {
		return MachineArtifact{}, false
	}
	targetPath := "pb-" + platform + "-" + architecture
	if platform == "windows" {
		targetPath += ".exe"
	}
	if platform == "darwin" {
		targetPath += ".pkg"
	}
	return MachineArtifact{Schema: "paperboat.tuf-target/v1", Kind: "pb", Version: version, Platform: platform, Architecture: architecture, RepositoryURL: s.artifactRepository, TargetPath: targetPath}, true
}

func New(store *db.DB, auditWriter *audit.Writer, policy Policy, seats SeatAuthorizer) *Service {
	if policy.OfflineAfter <= 0 {
		policy.OfflineAfter = 2 * time.Minute
	}
	return &Service{db: store, audit: auditWriter, policy: policy, seats: seats, now: time.Now, maxSessions: 20}
}

type PairingInput struct {
	Verifier, EnrollmentToken, Alias, Description, Platform, Architecture, WorkspaceRoot, PublicIdentityKey string
	SSHUser                                                                                                 string
	SSHPort                                                                                                 uint16
	RuntimeVersions                                                                                         json.RawMessage
	AcceptBetaPlatform                                                                                      bool
	CanReuseRuntimeIdentity                                                                                 bool
	KeepAwake                                                                                               bool
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
}

func (s *Service) StartEnrollment(ctx context.Context, userID, idempotencyKey string) (EnrollmentStart, error) {
	return s.StartEnrollmentWithOptions(ctx, userID, idempotencyKey, EnrollmentOptions{Shell: "posix"})
}

func (s *Service) StartEnrollmentWithOptions(ctx context.Context, userID, idempotencyKey string, options EnrollmentOptions) (EnrollmentStart, error) {
	userID, idempotencyKey = strings.TrimSpace(userID), strings.TrimSpace(idempotencyKey)
	if userID == "" || idempotencyKey == "" || len(idempotencyKey) > 128 {
		return EnrollmentStart{}, ErrIdempotencyKeyRequired
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
	Artifact                                 MachineArtifact
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
		build, buildErr := s.buildInstallationMaterial(ctx, userID, pairing, machine, operationKey, nil, in.CanReuseRuntimeIdentity, "", &in.Artifact, authorityGuard)
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
		if !s.authenticatedMachineInstallationMaterialMatches(current.InstallationConfigCiphertext, currentMachine.ID, currentMachine.EnvironmentID, in.InstallationGeneration, in.Artifact, current.AuthenticatedSetupHelperEnrollmentID) {
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

func (s *Service) validateAuthenticatedMachineInstallationMachine(machine UserMachine, in AuthenticatedMachineInstallationInput) (UserMachine, error) {
	expectedArtifact, artifactAvailable := s.machineArtifact(machine.Platform, machine.Architecture)
	if machine.SeatState != "occupied" ||
		machine.InstallationGeneration != in.InstallationGeneration || machine.PublicIdentityKey != in.PublicIdentityKey ||
		!artifactAvailable || in.Artifact != expectedArtifact {
		return UserMachine{}, ErrInvalidMachineInstallation
	}
	pairingInput := PairingInput{
		Verifier: in.Verifier, Alias: machine.Alias, Description: machine.Description, Platform: machine.Platform,
		Architecture: machine.Architecture, WorkspaceRoot: machine.WorkspaceRoot,
		PublicIdentityKey: machine.PublicIdentityKey, RuntimeVersions: machine.RuntimeVersions,
		SSHUser: in.SSHUser, SSHPort: in.SSHPort, CanReuseRuntimeIdentity: in.CanReuseRuntimeIdentity,
	}
	if err := s.validatePairing(pairingInput); err != nil {
		return UserMachine{}, ErrInvalidMachineInstallation
	}
	return machine, nil
}

func authenticatedMachineInstallationPairingMatches(pairing dbsqlc.UserMachinePairing, userID, cliClientSessionID string, machine UserMachine, in AuthenticatedMachineInstallationInput, verifierHash []byte, now time.Time) bool {
	return authenticatedMachineInstallationPairingFieldsMatch(pairing, userID, machine, in, verifierHash) &&
		pairing.AuthenticatedSetupCLISessionID.Valid && pairing.AuthenticatedSetupCLISessionID.String == cliClientSessionID &&
		slices.Contains([]string{"approved", "consumed"}, pairing.State) && now.Before(pairing.ExpiresAt)
}

func authenticatedMachineInstallationPairingFieldsMatch(pairing dbsqlc.UserMachinePairing, userID string, machine UserMachine, in AuthenticatedMachineInstallationInput, verifierHash []byte) bool {
	return pairing.AuthenticatedSetupCLISessionID.Valid &&
		pairing.AuthenticatedSetupOperationID.Valid && pairing.AuthenticatedSetupOperationID.String == in.OperationID &&
		pairing.AuthenticatedSetupGeneration.Valid && pairing.AuthenticatedSetupGeneration.Int64 == in.InstallationGeneration &&

		pairing.ApprovedByUserID.Valid && pairing.ApprovedByUserID.String == userID && pairing.UserMachineID.Valid && pairing.UserMachineID.String == machine.ID &&
		pairing.PublicIdentityKey.String == machine.PublicIdentityKey && bytes.Equal(pairing.VerifierHash, verifierHash) &&
		pairing.RequestedAlias.String == machine.Alias && pairing.RequestedDescription == machine.Description && pairing.Platform.String == machine.Platform && pairing.Architecture.String == machine.Architecture &&
		pairing.WorkspaceRoot.String == machine.WorkspaceRoot && bytes.Equal(pairing.RuntimeVersions, machine.RuntimeVersions) && pairing.CanReuseRuntimeIdentity == in.CanReuseRuntimeIdentity && pairing.KeepAwake == in.KeepAwake &&
		pairing.SshUser == (sql.NullString{String: in.SSHUser, Valid: in.SSHUser != ""}) && pairing.SshPort == (sql.NullInt32{Int32: int32(in.SSHPort), Valid: in.SSHPort != 0}) &&
		pairing.PublicIdentityKey.String == machine.PublicIdentityKey && bytes.Equal(pairing.VerifierHash, verifierHash)
}

func (s *Service) authenticatedMachineInstallationMaterialForRetry(pairing dbsqlc.UserMachinePairing, machine UserMachine, in AuthenticatedMachineInstallationInput) (string, error) {
	plaintext, err := secrets.Decrypt(s.encryptionKey, pairing.InstallationConfigCiphertext)
	if err != nil {
		return "", ErrMachineInstallationOperationConflict
	}
	binding, err := authenticatedMachineInstallationMaterialBindingFromPlaintext([]byte(plaintext))
	if err != nil || binding.MachineID != machine.ID || binding.EnvironmentID != machine.EnvironmentID ||
		binding.Generation != in.InstallationGeneration || binding.Artifact != in.Artifact ||
		binding.ReuseIdentity != in.CanReuseRuntimeIdentity ||
		!s.authenticatedMachineInstallationMaterialMatches(pairing.InstallationConfigCiphertext, machine.ID, machine.EnvironmentID, in.InstallationGeneration, in.Artifact, pairing.AuthenticatedSetupHelperEnrollmentID) {
		return "", ErrMachineInstallationOperationConflict
	}
	return authenticatedMachineInstallationMaterialSessionID([]byte(plaintext))
}

func authenticatedMachineInstallationMaterialSessionID(plaintext []byte) (string, error) {
	sessionJSON, err := clientSessionFromInstallationMaterial([]byte(plaintext))
	if err != nil || len(sessionJSON) == 0 {
		return "", ErrInvalidMachineInstallation
	}
	var session installationClientSession
	if err := json.Unmarshal(sessionJSON, &session); err != nil || session.Schema != "paperboat.cli-session/v1" ||
		strings.TrimSpace(session.SessionID) == "" || strings.TrimSpace(session.AccessToken) == "" || strings.TrimSpace(session.RefreshToken) == "" {
		return "", ErrInvalidMachineInstallation
	}
	return session.SessionID, nil
}

func (s *Service) authorizeAuthenticatedMachineInstallationRetry(ctx context.Context, tx *db.Tx, userID, cliClientSessionID string, machine UserMachine, pairing dbsqlc.UserMachinePairing, materialSessionID string) error {
	if pairing.AuthenticatedSetupCLISessionID.Valid && pairing.AuthenticatedSetupCLISessionID.String == cliClientSessionID {
		return nil
	}
	// Once bootstrap has switched the local profile, the original setup session
	// may be revoked and the pairing intentionally expired. Recovery is allowed
	// only through the exact active CLI session embedded in the encrypted
	// material, and that session must already be bound to this machine.
	if materialSessionID == "" || materialSessionID != cliClientSessionID || s.cliClientID == "" {
		return ErrInvalidMachineInstallation
	}
	session, err := tx.Queries().GetActiveClientSessionForUserMachine(ctx, dbsqlc.GetActiveClientSessionForUserMachineParams{
		ID: cliClientSessionID, UserID: userID, UserMachineID: sql.NullString{String: machine.ID, Valid: true},
	})
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidMachineInstallation
	}
	if err != nil {
		return err
	}
	if session.ClientID != s.cliClientID {
		return ErrInvalidMachineInstallation
	}
	return nil
}

type SetupInput struct {
	Alias, Description, Platform, Architecture, WorkspaceRoot, PublicIdentityKey string
	RuntimeVersions                                                              json.RawMessage
	AcceptBetaPlatform                                                           bool
	KeepAwake                                                                    bool
}

func optInUserMachineKeepAwake(ctx context.Context, tx *db.Tx, userID string, row dbsqlc.UserMachine, keepAwake bool) (dbsqlc.UserMachine, error) {
	if !keepAwake || row.AvailabilityMode == "keep_awake" {
		return row, nil
	}
	return tx.Queries().OptInUserMachineKeepAwake(ctx, dbsqlc.OptInUserMachineKeepAwakeParams{ID: row.ID, UserID: userID})
}

func (s *Service) Setup(ctx context.Context, userID string, in SetupInput) (UserMachine, error) {
	userID = strings.TrimSpace(userID)
	workspaceRoot, validWorkspace := canonicalWorkspaceRoot(in.Platform, in.WorkspaceRoot)
	publicKey, keyErr := base64.RawURLEncoding.DecodeString(strings.TrimSpace(in.PublicIdentityKey))
	if userID == "" || invalidMachineAlias(in.Alias) || invalidMachineDescription(in.Description) || !validMachineArchitecture(in.Architecture) ||
		!releases.SupportedPlatformArchitecture(strings.ToLower(strings.TrimSpace(in.Platform)), strings.ToLower(strings.TrimSpace(in.Architecture))) ||
		!validWorkspace ||
		!slices.Contains(s.policy.AllowedPlatforms, strings.ToLower(strings.TrimSpace(in.Platform))) ||
		keyErr != nil || len(publicKey) != ed25519.PublicKeySize {
		return UserMachine{}, ErrInvalidSetup
	}
	artifact, artifactAvailable := s.machineArtifact(strings.ToLower(strings.TrimSpace(in.Platform)), strings.ToLower(strings.TrimSpace(in.Architecture)))
	if !artifactAvailable || s.issuer == "" || s.helperListenPort == 0 {
		return UserMachine{}, ErrProvisioningUnavailable
	}
	in.WorkspaceRoot = workspaceRoot
	if len(in.RuntimeVersions) == 0 {
		in.RuntimeVersions = json.RawMessage(`{}`)
	}
	var result UserMachine
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := teams.Lock(ctx, tx); err != nil {
			return err
		}
		key := sql.NullString{String: strings.TrimSpace(in.PublicIdentityKey), Valid: true}
		row, err := tx.Queries().GetUserMachineByPublicIdentityForUpdate(ctx, key)
		if err == nil {
			if row.UserID != userID {
				return ErrMachineIdentityConflict
			}
			if row.OwnerTeamID.Valid {
				var canManage bool
				if err := tx.QueryRow(ctx, `SELECT machine_management_allowed($1,$2)`, userID, row.ID).Scan(&canManage); err != nil {
					return err
				}
				if !canManage {
					return ErrMachineIdentityConflict
				}
			}
			if row.Platform != strings.ToLower(strings.TrimSpace(in.Platform)) || row.Architecture != strings.ToLower(strings.TrimSpace(in.Architecture)) || row.WorkspaceRoot != in.WorkspaceRoot {
				return ErrInvalidSetup
			}
			row, err = tx.Queries().UpdateUserMachineMetadata(ctx, dbsqlc.UpdateUserMachineMetadataParams{ID: row.ID, UserID: userID, Alias: strings.TrimSpace(in.Alias), Description: strings.TrimSpace(in.Description), RuntimeVersions: in.RuntimeVersions})
			if err != nil {
				return err
			}

			row, err = s.completeAuthenticatedMachineInstallationTx(ctx, tx, userID, row, in)
			if err != nil {
				return err
			}

			row, err = optInUserMachineKeepAwake(ctx, tx, userID, row, in.KeepAwake)
			if err != nil {
				return err
			}

			if err := s.ensureHelperRoute(ctx, tx, row.ID, row.EnvironmentID); err != nil {
				return err
			}
			if _, err := s.ensureCurrentBandwidthPeriod(ctx, tx, row); err != nil {
				return err
			}

			result = mapMachine(row)
			return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "machine.setup_resumed", ResourceType: "machine", ResourceID: row.ID, IdempotencyKey: "machine.setup_resumed:" + row.ID + ":" + strconv.FormatInt(row.Version, 10)})
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		machineID := newID("machine")
		alias, err := allocateMachineAlias(ctx, tx.Queries(), userID, in.Alias)
		if err != nil {
			return err
		}
		row, err = tx.Queries().CreateEnrolledMachine(ctx, dbsqlc.CreateEnrolledMachineParams{
			ID: machineID, UserID: userID, EnvironmentID: machineID, Description: strings.TrimSpace(in.Description), Alias: alias,
			Platform: strings.ToLower(strings.TrimSpace(in.Platform)), Architecture: strings.ToLower(strings.TrimSpace(in.Architecture)),
			WorkspaceRoot: in.WorkspaceRoot, RuntimeVersions: in.RuntimeVersions, ConfiguredCapabilities: defaultConfiguredCapabilities(), PublicIdentityKey: key,
		})
		if userMachineTerminalSessionUniqueViolation(err) {
			return ErrMachineNameConflict
		}
		if err != nil {
			return err
		}
		if _, err := tx.Queries().CreateControlEnvironment(ctx, dbsqlc.CreateControlEnvironmentParams{ID: row.ID, WorkspaceID: row.ID, OwnerUserID: sql.NullString{String: userID, Valid: true}, DesiredState: "active"}); err != nil {
			return err
		}

		row, err = s.completeAuthenticatedMachineInstallationTx(ctx, tx, userID, row, in)
		if err != nil {
			return err
		}

		if err := s.ensureHelperRoute(ctx, tx, row.ID, row.EnvironmentID); err != nil {
			return err
		}
		if _, err := s.ensureCurrentBandwidthPeriod(ctx, tx, row); err != nil {
			return err
		}

		row, err = optInUserMachineKeepAwake(ctx, tx, userID, row, in.KeepAwake)
		if err != nil {
			return err
		}
		result = mapMachine(row)
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "machine.setup", ResourceType: "machine", ResourceID: row.ID, IdempotencyKey: "machine.setup:" + row.ID})
	})
	var aliasConflict *pgconn.PgError
	if errors.As(err, &aliasConflict) && aliasConflict.Code == "23505" && (aliasConflict.ConstraintName == "user_machines_owner_alias" || aliasConflict.ConstraintName == "user_machines_active_alias") {
		return UserMachine{}, ErrMachineNameConflict
	}

	if err == nil {
		result.Installation = &ClientInstallation{ControlURL: s.issuer, HelperListenAddress: fmt.Sprintf("127.0.0.1:%d", s.helperListenPort), Artifact: artifact}
	}
	return result, err
}

func (s *Service) completeAuthenticatedMachineInstallationTx(ctx context.Context, tx *db.Tx, userID string, row dbsqlc.UserMachine, in SetupInput) (dbsqlc.UserMachine, error) {
	var err error
	row, err = tx.Queries().UpdateUserMachineInstallation(ctx, dbsqlc.UpdateUserMachineInstallationParams{
		ID: row.ID, UserID: userID, Alias: strings.TrimSpace(in.Alias), Description: strings.TrimSpace(in.Description),
		WorkspaceRoot: in.WorkspaceRoot, RuntimeVersions: in.RuntimeVersions,
	})
	if err != nil {
		return dbsqlc.UserMachine{}, err
	}
	if row.SeatState == "released" {
		if s.seats == nil {
			return dbsqlc.UserMachine{}, ErrSeatUnavailable
		}
		if err := s.seats.ReserveUserMachineSeat(ctx, tx, userID); err != nil {
			return dbsqlc.UserMachine{}, err
		}
		if n, err := tx.Queries().OccupyUserMachineSeat(ctx, dbsqlc.OccupyUserMachineSeatParams{ID: row.ID, UserID: userID}); err != nil || n != 1 {
			if err != nil {
				return dbsqlc.UserMachine{}, err
			}
			return dbsqlc.UserMachine{}, ErrEnrollmentState
		}
		row.SeatState = "occupied"
	} else if row.SeatState != "occupied" {
		return dbsqlc.UserMachine{}, ErrEnrollmentState
	}
	if err := s.ensureHelperRoute(ctx, tx, row.ID, row.EnvironmentID); err != nil {
		return dbsqlc.UserMachine{}, err
	}
	if _, err := s.ensureCurrentBandwidthPeriod(ctx, tx, row); err != nil {
		return dbsqlc.UserMachine{}, err
	}
	return row, nil
}

func (s *Service) CreatePairing(ctx context.Context, in PairingInput) (Pairing, error) {
	if err := s.validatePairing(in); err != nil {
		return Pairing{}, err
	}
	in.WorkspaceRoot, _ = canonicalWorkspaceRoot(in.Platform, in.WorkspaceRoot)
	verifierHash := sha256.Sum256([]byte(in.Verifier))
	code, err := randomCode(8)
	if err != nil {
		return Pairing{}, err
	}
	if len(in.RuntimeVersions) == 0 {
		in.RuntimeVersions = json.RawMessage(`{}`)
	}
	expires := s.now().UTC().Add(s.policy.PairingLifetime)
	params := dbsqlc.CreateUserMachinePairingParams{VerifierHash: verifierHash[:], UserCode: sql.NullString{String: code, Valid: true}, RequestedAlias: sql.NullString{String: strings.TrimSpace(in.Alias), Valid: true}, RequestedDescription: strings.TrimSpace(in.Description), Platform: sql.NullString{String: strings.ToLower(strings.TrimSpace(in.Platform)), Valid: true}, Architecture: sql.NullString{String: strings.ToLower(strings.TrimSpace(in.Architecture)), Valid: true}, WorkspaceRoot: sql.NullString{String: in.WorkspaceRoot, Valid: true}, RuntimeVersions: in.RuntimeVersions, PublicIdentityKey: sql.NullString{String: strings.TrimSpace(in.PublicIdentityKey), Valid: true}, CanReuseRuntimeIdentity: in.CanReuseRuntimeIdentity, KeepAwake: in.KeepAwake, ExpiresAt: expires}
	if in.SSHUser != "" || in.SSHPort != 0 {
		params.SshUser = sql.NullString{String: strings.TrimSpace(in.SSHUser), Valid: true}
		params.SshPort = sql.NullInt32{Int32: int32(in.SSHPort), Valid: true}
	}
	var row dbsqlc.UserMachinePairing
	var enrollmentUserID string
	var enrollmentID string
	if strings.TrimSpace(in.EnrollmentToken) == "" {
		params.ID = newID("pairing")
		row, err = s.db.Queries().CreateUserMachinePairing(ctx, params)
	} else {
		token := strings.TrimSpace(in.EnrollmentToken)

		tokenHash := enrollmentTokenHash(token)
		err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			enrollment, err := tx.Queries().GetBootstrapPairingForTokenUpdate(ctx, tokenHash[:])
			if errors.Is(err, sql.ErrNoRows) {
				return ErrEnrollmentNotFound
			}
			if err != nil {
				return err
			}
			if enrollment.InstallationState != "awaiting_bootstrap" || !s.now().UTC().Before(enrollment.ExpiresAt) {
				return ErrEnrollmentState
			}
			enrollmentUserID = enrollment.BootstrapOwnerAccount.String
			enrollmentID = enrollment.ID
			if enrollment.ExpiresAt.Before(params.ExpiresAt) {
				params.ExpiresAt = enrollment.ExpiresAt
			}
			row, err = tx.Queries().ClaimBootstrapPairing(ctx, dbsqlc.ClaimBootstrapPairingParams{
				ID: enrollment.ID, VerifierHash: params.VerifierHash, UserCode: params.UserCode,
				RequestedAlias: params.RequestedAlias, RequestedDescription: params.RequestedDescription,
				Platform: params.Platform, Architecture: params.Architecture, WorkspaceRoot: params.WorkspaceRoot,
				RuntimeVersions: params.RuntimeVersions, PublicIdentityKey: params.PublicIdentityKey,
				SshUser: params.SshUser, SshPort: params.SshPort,
				CanReuseRuntimeIdentity: params.CanReuseRuntimeIdentity, KeepAwake: params.KeepAwake,
			})
			if errors.Is(err, sql.ErrNoRows) {
				return ErrEnrollmentState
			}
			if err != nil {
				return err
			}
			return nil
		})
	}
	if err != nil {
		return Pairing{}, err
	}
	// A dashboard-issued enrollment token is already authenticated by the
	// account owner. Consume the approval step immediately so a pasted command
	// is genuinely one-shot. The explicit pairing approval endpoint remains only
	// for requests whose device key is approved interactively by the account owner.
	if enrollmentUserID != "" {
		if _, err := s.approve(ctx, enrollmentUserID, row.UserCode.String); err != nil {
			// CreatePairing has already claimed the one-shot enrollment before
			// automatic approval. Approval runs in its own transaction because it
			// also serves the explicit dashboard approval endpoint; if it rolls
			// back, release the claim into the retryable state instead of leaving
			// the dashboard stuck at awaiting_approval.
			failedAt := s.now().UTC()
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cleanupCancel()
			if n, cleanupErr := s.db.Queries().FailBootstrapPairingApproval(cleanupCtx, dbsqlc.FailBootstrapPairingApprovalParams{
				PairingID: enrollmentID,
				UserID:    sql.NullString{String: enrollmentUserID, Valid: true},
				FailedAt:  failedAt,
			}); cleanupErr != nil || n != 1 {
				if cleanupErr == nil {
					cleanupErr = fmt.Errorf("updated %d rows, want 1", n)
				}
				return Pairing{}, errors.Join(err, fmt.Errorf("release failed machine enrollment: %w", cleanupErr))
			}
			return Pairing{}, err
		}
	}
	return Pairing{ID: row.ID, UserCode: row.UserCode.String, ExpiresAt: row.ExpiresAt}, nil
}

type UserMachine struct {
	Ownership              string                  `json:"ownership"`
	OwnerAccount           string                  `json:"owner_account,omitempty"`
	OwnerTeamID            string                  `json:"owner_team_id,omitempty"`
	Permissions            []string                `json:"permissions"`
	CanManage              bool                    `json:"can_manage"`
	Shared                 bool                    `json:"shared"`
	ID                     string                  `json:"id"`
	EnvironmentID          string                  `json:"environment_id"`
	Description            string                  `json:"description,omitempty"`
	Alias                  string                  `json:"alias"`
	Platform               string                  `json:"platform"`
	Architecture           string                  `json:"architecture"`
	WorkspaceRoot          string                  `json:"workspace_root"`
	State                  string                  `json:"state"`
	SeatState              string                  `json:"seat_state"`
	Online                 bool                    `json:"online"`
	RuntimeVersions        json.RawMessage         `json:"runtime_versions"`
	Capabilities           MachineCapabilities     `json:"capabilities"`
	MachineCapabilities    MachineCapabilityPolicy `json:"machine_capabilities"`
	PublicIdentityKey      string                  `json:"public_identity_key"`
	AssignedIP             string                  `json:"assigned_ip,omitempty"`
	NetworkApproved        bool                    `json:"network_approved"`
	InstallationGeneration int64                   `json:"installation_generation"`
	EnrolledAt             *time.Time              `json:"enrolled_at,omitempty"`
	LastSeenAt             *time.Time              `json:"last_seen_at,omitempty"`
	Availability           AvailabilityPolicy      `json:"availability"`
	RuntimeDiagnostics     RuntimeDiagnostics      `json:"runtime_diagnostics"`
	Installation           *ClientInstallation     `json:"installation,omitempty"`
}

type ClientInstallation struct {
	ControlURL          string          `json:"control_url"`
	HelperListenAddress string          `json:"helper_listen_address"`
	Artifact            MachineArtifact `json:"artifact"`
}

type CapabilityAvailability struct {
	Configured bool `json:"configured"`
	Observed   bool `json:"observed"`
}

type MachineCapabilities struct {
	FileReceive          CapabilityAvailability `json:"file_receive"`
	PreviewLaunch        CapabilityAvailability `json:"preview_launch"`
	TerminalHost         CapabilityAvailability `json:"terminal_host"`
	SessionHost          CapabilityAvailability `json:"session_host"`
	KeepAwake            CapabilityAvailability `json:"keep_awake"`
	EnvironmentInjection CapabilityAvailability `json:"environment_injection"`
}

func defaultConfiguredCapabilities() []string {
	return []string{"file_receive", "preview_launch", "terminal_host", "session_host", "ssh_host", "keep_awake", "environment_injection"}
}

type RuntimeDiagnostics struct {
	WorkerGeneration    uint64     `json:"worker_generation"`
	OSBootID            string     `json:"os_boot_id,omitempty"`
	WorkerServiceScope  string     `json:"worker_service_scope"`
	ConnectorState      string     `json:"connector_state"`
	ConnectorGeneration uint64     `json:"connector_generation"`
	ObservedAt          *time.Time `json:"observed_at,omitempty"`
}

// Overview is the dashboard-safe accounting snapshot. Bytes are returned as
// integers so every client can choose its own display units without affecting
// the authoritative accounting values.
type Overview struct {
	MachineLimit      *int32 `json:"machine_limit"`
	AvailableMachines *int32 `json:"available_machines"`
	MachineCount      int32  `json:"machine_count"`
	BootstrapCommand  string `json:"bootstrap_command,omitempty"`
}

func (s *Service) Overview(ctx context.Context, userID string) (Overview, error) {
	var count int32
	err := s.db.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.user_machines WHERE user_id=$1 AND owner_team_id IS NULL AND seat_state='occupied' AND deleted_at IS NULL`, userID).Scan(&count)
	out := Overview{MachineCount: count, BootstrapCommand: s.bootstrapCommand}
	if source, ok := s.seats.(interface {
		PersonalMachineQuota(context.Context) (*int32, error)
	}); ok {
		var err error
		out.MachineLimit, err = source.PersonalMachineQuota(ctx)
		if err != nil {
			return Overview{}, err
		}
		if out.MachineLimit != nil {
			available := max(int32(0), *out.MachineLimit-count)
			out.AvailableMachines = &available
		}
	}
	return out, err
}

func entitlementActive(state string, periodEnd, now time.Time) bool {
	return (state == "active" || state == "trialing") && now.Before(periodEnd)
}

type TerminalSession struct {
	CurrentDirectory  string     `json:"current_directory,omitempty"`
	ForegroundProcess string     `json:"foreground_process,omitempty"`
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	Title             string     `json:"title"`
	Machine           string     `json:"machine"`
	StartedIn         string     `json:"started_in"`
	IsDefault         bool       `json:"is_default"`
	State             string     `json:"state"`
	AttachedCount     *int       `json:"attached_count,omitempty"`
	LastActiveAt      *time.Time `json:"last_active_at,omitempty"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// BandwidthReservation is a trusted capacity grant. A data-plane relay must
// forward no more than GrantedBytes before requesting another grant.
type BandwidthReservation struct {
	GrantedBytes int64 `json:"granted_bytes"`
	Exhausted    bool  `json:"exhausted"`
}

func (s *Service) TransferDestinationDefault(ctx context.Context, userID string) (UserMachine, error) {
	row, err := s.db.Queries().GetUserTransferDestinationDefault(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return UserMachine{}, ErrNotFound
	}
	if err != nil {
		return UserMachine{}, err
	}
	return mapMachine(row), nil
}

func (s *Service) SetTransferDestinationDefault(ctx context.Context, userID, machineID string) (UserMachine, error) {
	var result UserMachine
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if _, err := tx.Queries().SetUserTransferDestinationDefault(ctx, dbsqlc.SetUserTransferDestinationDefaultParams{UserID: userID, MachineID: machineID}); errors.Is(err, sql.ErrNoRows) {
			return ErrTransferDestinationInvalid
		} else if err != nil {
			return err
		}
		row, err := tx.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: machineID, UserID: userID})
		if err != nil {
			return err
		}
		result = mapMachine(row)
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "transfer_destination.default_set", ResourceType: "user", ResourceID: userID, Metadata: map[string]any{"machine_id": machineID}})
	})
	return result, err
}

func (s *Service) ClearTransferDestinationDefault(ctx context.Context, userID string) error {
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		changed, err := tx.Queries().ClearUserTransferDestinationDefault(ctx, userID)
		if err != nil || changed == 0 {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "transfer_destination.default_cleared", ResourceType: "user", ResourceID: userID})
	})
}

func (s *Service) TerminalSessionTransferDestination(ctx context.Context, userID, sessionID string) (UserMachine, error) {
	boundContext, _, err := s.transferSessionHost(ctx, userID, sessionID)
	if errors.Is(err, ErrTerminalSessionNotFound) {
		return UserMachine{}, ErrNotFound
	}
	if err != nil {
		return UserMachine{}, err
	}
	var result UserMachine
	err = s.db.InTx(boundContext, func(ctx context.Context, tx *db.Tx) error {
		machineID, err := lockTransferSession(ctx, tx, userID, sessionID)
		if err != nil {
			return ErrNotFound
		}
		var destinationID sql.NullString
		_, teamID := workspaceTeamArgument(ctx)
		if err := tx.QueryRow(ctx, `SELECT transfer_destination_machine_id FROM user_machine_terminal_sessions
			WHERE id=$1 AND owner_account=$2 AND user_machine_id=$3
			AND owner_team_id IS NOT DISTINCT FROM $4::text AND deleted_at IS NULL`,
			sessionID, userID, machineID, teamID).Scan(&destinationID); errors.Is(err, sql.ErrNoRows) || !destinationID.Valid {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		row, err := getMachineForCapability(ctx, tx.Queries(), dbsqlc.GetUserMachineForCapabilityParams{
			ID: destinationID.String, UserID: userID, Capability: "files",
		})
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if row.State == "revoked" || row.State == "disconnected" || row.State == "deleted" || row.SeatState != "occupied" {
			return ErrNotFound
		}
		result = mapMachine(row)
		return nil
	})
	return result, err
}

func (s *Service) SetTerminalSessionTransferDestination(ctx context.Context, userID, sessionID, machineID string) (UserMachine, error) {
	var result UserMachine
	boundContext, _, err := s.transferSessionHost(ctx, userID, sessionID)
	if errors.Is(err, ErrTerminalSessionNotFound) {
		return UserMachine{}, ErrTransferDestinationInvalid
	}
	if err != nil {
		return UserMachine{}, err
	}
	err = s.db.InTx(boundContext, func(ctx context.Context, tx *db.Tx) error {
		ownerMachineID, err := lockTransferSession(ctx, tx, userID, sessionID)
		if err != nil {
			return ErrTransferDestinationInvalid
		}
		row, err := getMachineForCapability(ctx, tx.Queries(), dbsqlc.GetUserMachineForCapabilityParams{
			ID: machineID, UserID: userID, Capability: "files",
		})
		if errors.Is(err, sql.ErrNoRows) || row.State == "revoked" || row.State == "disconnected" || row.State == "deleted" || row.SeatState != "occupied" {
			return ErrTransferDestinationInvalid
		}
		if err != nil {
			return err
		}
		_, teamID := workspaceTeamArgument(ctx)
		updated, err := tx.Exec(ctx, `UPDATE user_machine_terminal_sessions
			SET transfer_destination_machine_id=$1, version=version+1, updated_at=now()
			WHERE id=$2 AND owner_account=$3 AND user_machine_id=$4
			AND owner_team_id IS NOT DISTINCT FROM $5::text AND deleted_at IS NULL`,
			machineID, sessionID, userID, ownerMachineID, teamID)
		if err != nil {
			return err
		}
		if updated.RowsAffected() != 1 {
			return ErrTransferDestinationInvalid
		}
		result = mapMachine(row)
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "transfer_destination.session_set", ResourceType: "terminal_session", ResourceID: sessionID, Metadata: map[string]any{"machine_id": machineID}})
	})
	return result, err
}

func (s *Service) ClearTerminalSessionTransferDestination(ctx context.Context, userID, sessionID string) error {
	boundContext, _, err := s.transferSessionHost(ctx, userID, sessionID)
	if errors.Is(err, ErrTerminalSessionNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.db.InTx(boundContext, func(ctx context.Context, tx *db.Tx) error {
		machineID, err := lockTransferSession(ctx, tx, userID, sessionID)
		if err != nil {
			return ErrNotFound
		}
		_, teamID := workspaceTeamArgument(ctx)
		updated, err := tx.Exec(ctx, `UPDATE user_machine_terminal_sessions
			SET transfer_destination_machine_id=NULL, version=version+1, updated_at=now()
			WHERE id=$1 AND owner_account=$2 AND user_machine_id=$3
			AND owner_team_id IS NOT DISTINCT FROM $4::text AND deleted_at IS NULL`, sessionID, userID, machineID, teamID)
		if err != nil {
			return err
		}
		if updated.RowsAffected() != 1 {
			return ErrNotFound
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "transfer_destination.session_cleared", ResourceType: "terminal_session", ResourceID: sessionID})
	})
}

func (s *Service) EligibleTerminalSessionTransferDestinations(ctx context.Context, userID, cliClientSessionID, sessionID string) ([]UserMachine, error) {
	if s.controlSigner == nil || cliClientSessionID == "" {
		return nil, ErrProvisioningUnavailable
	}
	boundContext, host, err := s.transferSessionHost(ctx, userID, sessionID)
	if err != nil {
		return nil, err
	}
	route, err := s.db.Queries().GetActiveHelperRouteForEnvironment(boundContext, host.EnvironmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProvisioningUnavailable
	}
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	expires := now.Add(2 * time.Minute)
	token, err := s.signProcessCredential(ctx, mint.CredentialInput{
		Issuer: s.issuer, Audience: "paperboat-machine", Subject: userID, JTI: newID("token"),
		IssuedAt: now, ExpiresAt: expires, CredentialClass: "terminal_operation", Scopes: []string{"terminal:operate"},
		EnvironmentID: host.EnvironmentID, MachineID: host.ID, UserID: userID, CLIClientSessionID: cliClientSessionID, SessionID: sessionID,
	})
	if err != nil {
		return nil, err
	}
	httpIngressURL, ingressErr := s.runtimeIngressURL(boundContext, host.EnvironmentID, route.PublicHost)
	if ingressErr != nil {
		return nil, ingressErr
	}
	snapshot, err := s.controlRuntime.Terminal(boundContext, httpIngressURL, token, "transfer-destinations", sessionID, newID("operation"))
	if err != nil {
		return nil, err
	}
	result := make([]UserMachine, 0, len(snapshot.EligibleTransferDestinationMachineIDs))
	for _, machineID := range snapshot.EligibleTransferDestinationMachineIDs {
		machine, allowed, lookupErr := s.transferDestinationMachine(boundContext, userID, machineID)
		if lookupErr != nil {
			return nil, lookupErr
		}
		if allowed {
			result = append(result, machine)
		}
	}
	return result, nil
}

func (s *Service) OwnedEligibleMachines(ctx context.Context, userID string, machineIDs []string) ([]UserMachine, error) {
	result := make([]UserMachine, 0, len(machineIDs))
	for _, machineID := range machineIDs {
		row, err := s.db.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: machineID, UserID: userID})
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if row.State != "revoked" && row.State != "disconnected" && row.State != "deleted" && row.SeatState == "occupied" {
			result = append(result, mapMachine(row))
		}
	}
	return result, nil
}

type ConnectionDescriptor struct {
	Schema            string         `json:"schema,omitempty"`
	Capabilities      []string       `json:"capabilities,omitempty"`
	Issuer            string         `json:"issuer,omitempty"`
	UserMachineID     string         `json:"machine_id"`
	UserMachineState  string         `json:"machine_state"`
	MachineGeneration int64          `json:"machine_generation"`
	Connectable       bool           `json:"connectable"`
	ExpiresAt         time.Time      `json:"expires_at"`
	Environment       map[string]any `json:"environment,omitempty"`
	Terminal          map[string]any `json:"terminal,omitempty"`
	FileTransfer      map[string]any `json:"file_transfer,omitempty"`
	Status            string         `json:"status,omitempty"`
	Reason            string         `json:"reason,omitempty"`
	RetryAfterSeconds int            `json:"retry_after_seconds,omitempty"`
}

type ExecDescriptor struct {
	OperationID string         `json:"operation_id"`
	Environment map[string]any `json:"environment"`
	Endpoints   map[string]any `json:"endpoints"`
	Auth        map[string]any `json:"auth"`
	ExpiresAt   time.Time      `json:"expires_at"`
}

type SSHDescriptor = ExecDescriptor

func (s *Service) ExecDescriptor(ctx context.Context, userID, sourceMachineID, userMachineID, cliClientSessionID, operationID string) (ExecDescriptor, error) {
	return s.operationDescriptor(ctx, userID, sourceMachineID, userMachineID, cliClientSessionID, operationID, "exec_operation", "exec:operate", ErrExecOperationInvalid)
}

func (s *Service) SSHDescriptor(ctx context.Context, userID, sourceMachineID, userMachineID, cliClientSessionID, operationID string) (SSHDescriptor, error) {
	return s.operationDescriptor(ctx, userID, sourceMachineID, userMachineID, cliClientSessionID, operationID, "ssh_operation", "ssh:operate", ErrSSHOperationInvalid)
}

func (s *Service) operationDescriptor(ctx context.Context, userID, sourceMachineID, userMachineID, cliClientSessionID, operationID, credentialClass, scope string, invalidOperationErr error) (ExecDescriptor, error) {
	if !execOperationIDPattern.MatchString(operationID) {
		return ExecDescriptor{}, invalidOperationErr
	}
	if sourceMachineID == "" || cliClientSessionID == "" {
		return ExecDescriptor{}, ErrNotFound
	}
	if _, err := s.db.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: sourceMachineID, UserID: userID}); err != nil {
		return ExecDescriptor{}, ErrNotFound
	}
	capability := "exec"
	if credentialClass == "ssh_operation" {
		capability = "managed_ssh"
	}
	row, err := getMachineForCapability(ctx, s.db.Queries(), dbsqlc.GetUserMachineForCapabilityParams{ID: userMachineID, UserID: userID, Capability: capability})
	if errors.Is(err, sql.ErrNoRows) {
		return ExecDescriptor{}, ErrNotFound
	}
	if err != nil {
		return ExecDescriptor{}, err
	}
	incoming := "terminal_host"
	if capability == "managed_ssh" {
		incoming = "ssh_host"
	}
	if row.State != "online" || row.SeatState != "occupied" || !row.Online || !slices.Contains(row.ObservedCapabilities, incoming) || !slices.Contains(row.ConfiguredCapabilities, incoming) {
		return ExecDescriptor{}, ErrMachineCapabilityUnavailable
	}
	if credentialClass == "ssh_operation" {
		target, targetErr := s.db.Queries().GetMachineSSHTargetForUpdate(ctx, row.ID)
		if targetErr != nil || target.MachineGeneration != row.InstallationGeneration {
			if targetErr != nil && !errors.Is(targetErr, sql.ErrNoRows) {
				return ExecDescriptor{}, targetErr
			}
			return ExecDescriptor{}, ErrMachineCapabilityUnavailable
		}
		keys, keysErr := s.db.Queries().GetActiveMachineSSHHostKeySetForUpdate(ctx, row.ID)
		if keysErr != nil || keys.MachineGeneration != row.InstallationGeneration {
			if keysErr != nil && !errors.Is(keysErr, sql.ErrNoRows) {
				return ExecDescriptor{}, keysErr
			}
			return ExecDescriptor{}, ErrMachineCapabilityUnavailable
		}
	}
	route, err := s.db.Queries().GetActiveHelperRouteForEnvironment(ctx, row.EnvironmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return ExecDescriptor{}, ErrMachineCapabilityUnavailable
	}
	if err != nil {
		return ExecDescriptor{}, err
	}
	if s.controlSigner == nil {
		return ExecDescriptor{}, errors.New("user-machine exec credential issuer is unavailable")
	}
	ttl := s.ttl
	if ttl <= 0 || ttl > 5*time.Minute {
		ttl = 5 * time.Minute
	}
	issuedAt := s.now().UTC()
	expiresAt := issuedAt.Add(ttl)
	operationJTI := newID("token")
	accessSessionID := newID("session")
	token, err := s.signProcessCredential(ctx, mint.CredentialInput{
		Issuer: s.issuer, Audience: "paperboat-machine", Subject: userID, JTI: operationJTI,
		IssuedAt: issuedAt, ExpiresAt: expiresAt, CredentialClass: credentialClass, Scopes: []string{scope},
		EnvironmentID: row.EnvironmentID, MachineID: row.ID, SourceMachineID: sourceMachineID, UserID: userID, CLIClientSessionID: cliClientSessionID, OperationID: operationID, AssignmentID: accessSessionID,
	})
	if err != nil {
		return ExecDescriptor{}, err
	}
	httpIngressURL, ingressErr := s.runtimeIngressURL(ctx, row.EnvironmentID, route.PublicHost)
	if ingressErr != nil {
		return ExecDescriptor{}, ingressErr
	}
	if err := s.createAccessSession(ctx, dbsqlc.CreateUserMachineAccessSessionParams{
		ID: accessSessionID, UserMachineID: row.ID, UserID: userID, EnvironmentID: row.EnvironmentID,
		CLIClientSessionID: cliClientSessionID, HttpBaseUrl: httpIngressURL,
		HelperTerminalSessionID: operationJTI, HelperFileSessionID: "", ExpiresAt: expiresAt, Capabilities: []string{capability}, OperationID: operationID,
	}, machineCredentialSigned); err != nil {
		return ExecDescriptor{}, err
	}
	return ExecDescriptor{
		OperationID: operationID,
		Environment: map[string]any{"id": row.EnvironmentID, "kind": accessdescriptor.EnvironmentMachine, "resource_id": row.ID, "alias": row.Alias, "state": "ready", "root": row.WorkspaceRoot},
		Endpoints:   machineTerminalEndpoints(runtimeWebsocketBase(httpIngressURL)),
		Auth:        map[string]any{"method": "bearer", "token": token, "expires_at": expiresAt, "scopes": []string{scope}, "access_session_id": accessSessionID},
		ExpiresAt:   expiresAt,
	}, nil
}

type FileTransferDescriptor struct {
	Endpoint             string                              `json:"endpoint"`
	SourceMachineID      string                              `json:"source_machine_id"`
	DestinationMachineID string                              `json:"destination_machine_id"`
	InitiatingUserID     string                              `json:"initiating_user_id"`
	Auth                 map[string]any                      `json:"auth"`
	Policy               accessdescriptor.FileTransferPolicy `json:"policy"`
}

func (s *Service) FileTransferDescriptor(ctx context.Context, userID, sourceMachineID, destinationMachineID, cliClientSessionID, sessionID string) (FileTransferDescriptor, error) {
	return s.fileTransferDescriptor(ctx, userID, sourceMachineID, destinationMachineID, cliClientSessionID, sessionID, "", "")
}

func (s *Service) FileTransferDescriptorForRequest(ctx context.Context, userID, sourceMachineID, destinationMachineID, cliClientSessionID, sessionID, requestID, manifestDigest string) (FileTransferDescriptor, error) {
	return s.fileTransferDescriptor(ctx, userID, sourceMachineID, destinationMachineID, cliClientSessionID, sessionID, requestID, manifestDigest)
}

func (s *Service) fileTransferDescriptor(ctx context.Context, userID, sourceMachineID, destinationMachineID, cliClientSessionID, sessionID, requestID, manifestDigest string) (FileTransferDescriptor, error) {
	if sourceMachineID == "" || destinationMachineID == "" || sourceMachineID == destinationMachineID || cliClientSessionID == "" || s.controlSigner == nil {
		return FileTransferDescriptor{}, ErrNotFound
	}
	if _, err := s.db.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: sourceMachineID, UserID: userID}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FileTransferDescriptor{}, ErrNotFound
		}
		return FileTransferDescriptor{}, err
	}
	if sessionID != "" {
		owned, err := s.db.Queries().UserOwnsTerminalSession(ctx, dbsqlc.UserOwnsTerminalSessionParams{SessionID: sessionID, UserID: userID})
		if err != nil {
			return FileTransferDescriptor{}, err
		}
		if !owned {
			return FileTransferDescriptor{}, ErrTerminalSessionNotFound
		}
	}
	destination, err := getMachineForCapability(ctx, s.db.Queries(), dbsqlc.GetUserMachineForCapabilityParams{ID: destinationMachineID, UserID: userID, Capability: "files"})
	if errors.Is(err, sql.ErrNoRows) {
		return FileTransferDescriptor{}, ErrNotFound
	}
	if err != nil {
		return FileTransferDescriptor{}, err
	}
	if sessionID == "" && !slices.Contains(destination.ConfiguredCapabilities, "file_receive") {
		return FileTransferDescriptor{}, ErrMachineCapabilityUnavailable
	}
	if sessionID == "" && (!destination.Online || !slices.Contains(destination.ObservedCapabilities, "file_receive")) {
		return FileTransferDescriptor{}, ErrMachineOffline
	}
	if destination.State == "revoked" || destination.State == "disconnected" || destination.State == "deleted" {
		return FileTransferDescriptor{}, ErrNotFound
	}
	var acceptedRequest *teaminbox.Request
	if destination.UserID != userID {
		if s.teamInbox == nil || requestID == "" || manifestDigest == "" {
			return FileTransferDescriptor{}, teaminbox.ErrPending
		}
		accepted, acceptErr := s.teamInbox.Consume(ctx, userID, requestID, manifestDigest)
		if acceptErr != nil {
			return FileTransferDescriptor{}, acceptErr
		}
		if accepted.SourceMachineID != sourceMachineID || accepted.DestinationMachineID != destinationMachineID {
			return FileTransferDescriptor{}, teaminbox.ErrConflict
		}
		acceptedRequest = &accepted
	}
	routeMachine := destination
	if sessionID != "" {
		host, hostErr := s.db.Queries().GetUserMachineTerminalSessionHostForUser(ctx, dbsqlc.GetUserMachineTerminalSessionHostForUserParams{SessionID: sessionID, UserID: userID})
		if hostErr == nil {
			routeMachine = host
		} else if !errors.Is(hostErr, sql.ErrNoRows) {
			return FileTransferDescriptor{}, hostErr
		}
	}
	if routeMachine.State == "revoked" || routeMachine.State == "disconnected" || routeMachine.State == "deleted" || sessionID != "" && !slices.Contains(routeMachine.ObservedCapabilities, "terminal_host") {
		return FileTransferDescriptor{}, ErrProvisioningUnavailable
	}
	route, err := s.db.Queries().GetActiveHelperRouteForEnvironment(ctx, routeMachine.EnvironmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return FileTransferDescriptor{}, ErrProvisioningUnavailable
	}
	if err != nil {
		return FileTransferDescriptor{}, err
	}
	ttl := s.ttl
	if ttl <= 0 || ttl > 5*time.Minute {
		ttl = 5 * time.Minute
	}
	expiresAt := s.now().UTC().Add(ttl)
	jti := newID("token")
	accessSessionID := newID("session")
	operationID := sessionID
	if requestID != "" {
		operationID = requestID
	}
	credentialInput := mint.CredentialInput{
		Issuer: s.issuer, Audience: "paperboat-machine", Subject: userID, JTI: jti,
		IssuedAt: s.now().UTC(), ExpiresAt: expiresAt, CredentialClass: "file_transfer", Scopes: []string{"file:transfer"},
		EnvironmentID: routeMachine.EnvironmentID, MachineID: routeMachine.ID, SourceMachineID: sourceMachineID,
		UserID: userID, CLIClientSessionID: cliClientSessionID, SessionID: sessionID, OperationID: operationID, AssignmentID: accessSessionID,
	}
	if acceptedRequest != nil {
		credentialInput.RequestID, credentialInput.IdempotencyKey, credentialInput.RequestHash = acceptedRequest.RequestID, acceptedRequest.BatchID, acceptedRequest.ManifestDigest
	}
	token, err := s.controlSigner.SignCredential(credentialInput)
	if err != nil {
		return FileTransferDescriptor{}, err
	}
	httpIngressURL, ingressErr := s.runtimeIngressURL(ctx, routeMachine.EnvironmentID, route.PublicHost)
	if ingressErr != nil {
		return FileTransferDescriptor{}, ingressErr
	}
	httpBaseURL := httpIngressURL
	if err := s.createAccessSession(ctx, dbsqlc.CreateUserMachineAccessSessionParams{
		Capabilities: []string{"files"}, OperationID: operationID, ID: accessSessionID, UserMachineID: routeMachine.ID, UserID: userID, EnvironmentID: routeMachine.EnvironmentID,
		CLIClientSessionID: cliClientSessionID, HttpBaseUrl: httpBaseURL, HelperFileSessionID: jti, ExpiresAt: expiresAt,
	}, machineCredentialSigned); err != nil {
		return FileTransferDescriptor{}, err
	}
	return FileTransferDescriptor{
		Endpoint: httpBaseURL + "/v1/file-transfers", SourceMachineID: sourceMachineID,
		DestinationMachineID: destination.ID, InitiatingUserID: userID, Policy: s.fileTransferPolicy,
		Auth: map[string]any{"method": "bearer", "token": token, "expires_at": expiresAt, "scopes": []string{"file:transfer"}, "access_session_id": accessSessionID},
	}, nil
}

func (r ConnectionDescriptor) MarshalJSON() ([]byte, error) {
	if r.Schema != accessdescriptor.SchemaV1 {
		return nil, errors.New("connection descriptor schema is required")
	}
	environment, ok := r.Environment["id"].(string)
	if !ok || environment == "" {
		return nil, errors.New("canonical environment descriptor is incomplete")
	}
	env := accessdescriptor.Environment{
		ID: environment, Kind: stringValue(r.Environment, "kind"), ResourceID: stringValue(r.Environment, "resource_id"),
		Alias: stringValue(r.Environment, "alias"), State: stringValue(r.Environment, "state"), Root: stringValue(r.Environment, "root"),
	}
	if env.Kind == "" || env.ResourceID == "" || env.Alias == "" || env.State == "" {
		return nil, errors.New("canonical environment descriptor is incomplete")
	}
	out := accessdescriptor.Descriptor{Schema: r.Schema, Issuer: r.Issuer, Connectable: r.Connectable, ExpiresAt: r.ExpiresAt, Environment: env, Capabilities: slices.Clone(r.Capabilities), Status: r.Status, Reason: r.Reason, RetryAfterSeconds: r.RetryAfterSeconds}
	if r.Terminal != nil && r.Terminal["auth"] != nil {
		terminal, err := decodeCanonical[accessdescriptor.Terminal](r.Terminal)
		if err != nil {
			return nil, err
		}
		out.Terminal = &terminal
	}
	if r.FileTransfer != nil && r.FileTransfer["auth"] != nil {
		transfer, err := decodeCanonical[accessdescriptor.FileTransfer](r.FileTransfer)
		if err != nil {
			return nil, err
		}
		out.FileTransfer = &transfer
	}
	return json.Marshal(out)
}

func stringValue(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return result
}

func decodeCanonical[T any](value any) (T, error) {
	var out T
	b, err := json.Marshal(value)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(b, &out)
	return out, err
}

func machineConnectionState(connectable bool, state string) string {
	if connectable {
		return "ready"
	}
	switch state {
	case "deleted":
		return "deleted"
	case "revoked", "disconnected":
		return "revoked"
	case "starting", "provisioning":
		return "starting"
	default:
		return "offline"
	}
}

func setCanonicalMachineIdentity(response *ConnectionDescriptor, row dbsqlc.UserMachine) {
	response.MachineGeneration = row.InstallationGeneration
	response.Schema = accessdescriptor.SchemaV1
	response.Capabilities = []string{accessdescriptor.CapabilityTerminal, accessdescriptor.CapabilityFileTransfer, accessdescriptor.CapabilityPreview}
	response.Environment = map[string]any{"id": row.EnvironmentID, "kind": accessdescriptor.EnvironmentMachine, "resource_id": row.ID, "alias": row.Alias, "state": machineConnectionState(response.Connectable, row.State), "root": row.WorkspaceRoot}
}

// ConnectSharedTerminalSession issues only the exact current session role. It
// deliberately bypasses machine capability grants: the session grant is the
// complete authority and confers no file, ENV, exec, or resharing access.
func (s *Service) ConnectSharedTerminalSession(ctx context.Context, userID, sourceMachineID, cliClientSessionID, sessionID string) (ConnectionDescriptor, error) {
	if sourceMachineID == "" || cliClientSessionID == "" || sessionID == "" {
		return ConnectionDescriptor{}, ErrNotFound
	}
	if _, err := s.db.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: sourceMachineID, UserID: userID}); exactNoRows(err) {
		return ConnectionDescriptor{}, ErrNotFound
	} else if err != nil {
		return ConnectionDescriptor{}, err
	}
	teamFilter := ""
	if scope, selected := workspace.FromContext(ctx); selected {
		if scope.TeamID == "" {
			return ConnectionDescriptor{}, ErrTerminalSessionNotFound
		}
		teamFilter = scope.TeamID
	}
	decision, err := teams.NewService(s.db).ResolveTerminalSessionInTeam(ctx, userID, sessionID, teamFilter)
	if err != nil {
		if err == teams.ErrNotFound {
			return ConnectionDescriptor{}, ErrTerminalSessionNotFound
		}
		return ConnectionDescriptor{}, err
	}
	row, err := s.db.Queries().GetUserMachineForSharedTerminalSession(ctx, dbsqlc.GetUserMachineForSharedTerminalSessionParams{SessionID: sessionID, UserID: userID})
	if errors.Is(err, sql.ErrNoRows) {
		return ConnectionDescriptor{}, ErrTerminalSessionNotFound
	}
	if err != nil {
		return ConnectionDescriptor{}, err
	}
	if !slices.Contains(row.ConfiguredCapabilities, "terminal_host") {
		return ConnectionDescriptor{}, ErrMachineCapabilityUnavailable
	}
	ttl := s.ttl
	if ttl <= 0 || ttl > 5*time.Minute {
		ttl = 5 * time.Minute
	}
	expires := s.now().UTC().Add(ttl)
	response := ConnectionDescriptor{Issuer: s.issuer, UserMachineID: row.ID, UserMachineState: row.State, ExpiresAt: expires, Status: "connector_connecting", Reason: "connector_offline", RetryAfterSeconds: 2}
	setCanonicalMachineIdentity(&response, row)
	if row.State == "revoked" || row.State == "disconnected" || row.State == "deleted" {
		response.Status, response.Reason = "user_machine_revoked", "access_revoked"
		return response, nil
	}
	if !row.Online || !slices.Contains(row.ObservedCapabilities, "terminal_host") {
		response.Status, response.Reason = "machine_offline", "terminal_host_unavailable"
		return response, nil
	}
	route, err := s.db.Queries().GetActiveHelperRouteForEnvironment(ctx, row.EnvironmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return response, nil
	}
	if err != nil {
		return ConnectionDescriptor{}, err
	}
	if s.controlSigner == nil {
		return ConnectionDescriptor{}, errors.New("user-machine credential issuer is unavailable")
	}
	now := s.now().UTC()
	snapshotToken, err := s.signProcessCredential(ctx, mint.CredentialInput{Issuer: s.issuer, Audience: "paperboat-machine", Subject: decision.OwnerAccount, JTI: newID("token"), IssuedAt: now, ExpiresAt: now.Add(time.Minute), CredentialClass: "terminal_operation", Scopes: []string{"terminal:operate"}, EnvironmentID: row.EnvironmentID, MachineID: row.ID, UserID: decision.OwnerAccount, CLIClientSessionID: "control-plane", SessionID: sessionID})
	if err != nil {
		return ConnectionDescriptor{}, err
	}
	httpIngressURL, ingressErr := s.runtimeIngressURL(ctx, row.EnvironmentID, route.PublicHost)
	if ingressErr != nil {
		return ConnectionDescriptor{}, ingressErr
	}
	snapshot, err := s.controlRuntime.Terminal(ctx, httpIngressURL, snapshotToken, "snapshot", sessionID, newID("operation"))
	if err != nil {
		return ConnectionDescriptor{}, err
	}
	if _, err = s.db.SQL().ExecContext(ctx, `UPDATE user_machine_terminal_sessions SET runtime_state=$2,closed_at=CASE WHEN $2 IN ('closed','exited','deleted') THEN coalesce(closed_at,now()) ELSE NULL END,launch_cwd=coalesce(nullif($3,''),launch_cwd),last_runtime_sync_at=now(),last_runtime_sequence=$4,last_activity_at=CASE WHEN $5::timestamptz IS NULL THEN last_activity_at ELSE greatest(coalesce(last_activity_at,$5),$5) END,updated_at=now() WHERE id=$1`, sessionID, snapshot.State, cleanTerminalMetadata(snapshot.CWD, 1024), int64(snapshot.LatestSequence), snapshot.LastActiveAt); err != nil {
		return ConnectionDescriptor{}, err
	}
	if snapshot.State == "closed" || snapshot.State == "exited" || snapshot.State == "deleted" {
		return ConnectionDescriptor{}, ErrTerminalSessionNotFound
	}
	if snapshot.Generation != decision.ProcessGeneration {
		if fenceErr := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			if err := teams.Lock(ctx, tx); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE team_resource_bindings SET active=false,generation=generation+1 WHERE team_id=$1 AND resource_kind='terminal_session' AND resource_id=$2 AND active AND terminal_process_generation=$3`, decision.TeamID, sessionID, decision.ProcessGeneration)
			return err
		}); fenceErr != nil {
			return ConnectionDescriptor{}, fenceErr
		}
		return ConnectionDescriptor{}, ErrTerminalSessionNotFound
	}
	var cwd string
	if decision.TargetKind != "machine" {
		return ConnectionDescriptor{}, ErrTerminalSessionNotFound
	}
	session, e := s.db.Queries().GetSharedUserMachineTerminalSession(ctx, dbsqlc.GetSharedUserMachineTerminalSessionParams{SessionID: sessionID, UserID: userID})
	if exactNoRows(e) {
		return ConnectionDescriptor{}, ErrTerminalSessionNotFound
	}
	if e != nil {
		return ConnectionDescriptor{}, e
	}
	cwd = session.LaunchCwd
	accessID, jti := newID("session"), newID("token")
	scope := "terminal:view"
	if decision.Role == "interactive" {
		scope = "terminal:control"
	}
	token, err := s.signProcessCredential(ctx, mint.CredentialInput{Issuer: s.issuer, Audience: "paperboat-machine", Subject: userID, JTI: jti, IssuedAt: s.now().UTC(), ExpiresAt: expires, CredentialClass: "terminal_operation", Scopes: []string{scope}, EnvironmentID: row.EnvironmentID, MachineID: row.ID, SourceMachineID: sourceMachineID, UserID: userID, AccountID: userID, CLIClientSessionID: cliClientSessionID, SessionID: sessionID, AssignmentID: accessID, ExpectedGeneration: int64(decision.ProcessGeneration)})
	if err != nil {
		return ConnectionDescriptor{}, err
	}
	params := dbsqlc.CreateUserMachineAccessSessionParams{ID: accessID, UserMachineID: row.ID, UserID: userID, EnvironmentID: row.EnvironmentID, CLIClientSessionID: cliClientSessionID, HttpBaseUrl: httpIngressURL, HelperTerminalSessionID: jti, ExpiresAt: expires, TeamID: sql.NullString{String: decision.TeamID, Valid: true}, Capabilities: []string{}, OperationID: sessionID, TerminalSessionID: sessionID, TerminalRole: decision.Role, TerminalGrantGeneration: sql.NullInt64{Int64: int64(decision.GrantGeneration), Valid: true}, TerminalBindingGeneration: sql.NullInt64{Int64: int64(decision.BindingGeneration), Valid: true}, TerminalMembershipGeneration: sql.NullInt64{Int64: int64(decision.MembershipGeneration), Valid: true}, TerminalTeamGeneration: sql.NullInt64{Int64: int64(decision.TeamGeneration), Valid: true}}
	if err = s.createSharedTerminalAccessSession(ctx, params, sourceMachineID, decision); err != nil {
		cleanupErr := s.revokeCredentialSessions(ctx, machineAccessSession{CredentialKind: machineCredentialSigned, UserID: userID, UserMachineID: row.ID, EnvironmentID: row.EnvironmentID, CLIClientSessionID: cliClientSessionID, HTTPBaseURL: httpIngressURL, TerminalSessionID: jti}, "access_session_persistence_failed")
		return ConnectionDescriptor{}, errors.Join(err, cleanupErr)
	}
	response.Connectable, response.Status, response.Reason, response.RetryAfterSeconds = true, "ready", "ready", 0
	setCanonicalMachineIdentity(&response, row)
	auth := map[string]any{"method": "bearer", "token": token, "expires_at": expires, "scopes": []string{scope}, "access_session_id": accessID}
	response.Terminal = map[string]any{"protocol": "paperboat.terminal.v1", "endpoints": machineTerminalEndpoints(runtimeWebsocketBase(httpIngressURL)), "session_id": sessionID, "cwd": cwd, "role": decision.Role, "auth": auth}
	return response, nil
}

func (s *Service) createSharedTerminalAccessSession(ctx context.Context, in dbsqlc.CreateUserMachineAccessSessionParams, sourceMachineID string, want teams.TerminalSessionDecision) error {
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := teams.Lock(ctx, tx); err != nil {
			return err
		}
		var ready bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM user_machines source JOIN cli_client_sessions cli ON cli.id=$3 AND cli.user_id=$1 AND cli.state='active' AND cli.revoked_at IS NULL JOIN user_machines target ON target.id=$4 WHERE source.id=$2 AND source.user_id=$1 AND source.deleted_at IS NULL AND source.revoked_at IS NULL AND target.state='online' AND target.online AND target.deleted_at IS NULL AND target.revoked_at IS NULL AND target.configured_capabilities @> ARRAY['terminal_host'] AND target.observed_capabilities @> ARRAY['terminal_host'])`, in.UserID, sourceMachineID, in.CLIClientSessionID, in.UserMachineID).Scan(&ready); err != nil {
			return err
		}
		if !ready {
			return ErrTerminalSessionNotFound
		}
		var team, role string
		var tg, mg, bg, gg, pg uint64
		err := tx.QueryRow(ctx, `SELECT d.team_id,d.role,d.team_generation,d.membership_generation,d.binding_generation,d.grant_generation,d.process_generation FROM (SELECT b.team_id,terminal_session_role($1,$2) role,t.generation team_generation,m.membership_generation,b.generation binding_generation,b.terminal_process_generation process_generation,CASE WHEN sg.account_id IS NOT NULL THEN sg.generation ELSE ag.generation END grant_generation FROM team_resource_bindings b JOIN teams t USING(team_id) JOIN team_members m ON m.team_id=b.team_id AND m.account_id=$1 LEFT JOIN team_terminal_session_grants sg ON sg.team_id=b.team_id AND sg.terminal_session_id=$2 AND sg.audience='selected_member' AND sg.account_id=$1 LEFT JOIN team_terminal_session_grants ag ON ag.team_id=b.team_id AND ag.terminal_session_id=$2 AND ag.audience='all_members' WHERE b.resource_kind='terminal_session' AND b.resource_id=$2 AND b.active AND m.active AND t.deleted_at IS NULL) d WHERE d.role IN ('viewer','interactive') ORDER BY d.team_id LIMIT 1`, in.UserID, want.SessionID).Scan(&team, &role, &tg, &mg, &bg, &gg, &pg)
		if exactNoRows(err) {
			return ErrTerminalSessionNotFound
		}
		if err != nil {
			return err
		}
		if team != want.TeamID || role != want.Role || tg != want.TeamGeneration || mg != want.MembershipGeneration || bg != want.BindingGeneration || gg != want.GrantGeneration || pg != want.ProcessGeneration {
			return ErrTerminalSessionNotFound
		}
		in.CredentialKind = string(machineCredentialSigned)
		if err := tx.Queries().CreateUserMachineAccessSession(ctx, in); err != nil {
			return err
		}
		return teams.AuditTx(ctx, tx, in.UserID, want.TeamID, "terminal_session_access_authorized", in.ID, map[string]any{"terminal_session_id": want.SessionID, "role": want.Role, "access_session_id": in.ID})
	})
}

func (s *Service) Connect(ctx context.Context, userID, sourceMachineID, userMachineID, cliClientSessionID string) (ConnectionDescriptor, error) {
	return s.ConnectTerminalSession(ctx, userID, sourceMachineID, userMachineID, cliClientSessionID, "")
}

func (s *Service) ConnectTerminalSession(ctx context.Context, userID, sourceMachineID, userMachineID, cliClientSessionID, terminalSessionID string) (ConnectionDescriptor, error) {
	if sourceMachineID == "" {
		return ConnectionDescriptor{}, ErrNotFound
	}
	if _, err := s.db.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: sourceMachineID, UserID: userID}); exactNoRows(err) {
		return ConnectionDescriptor{}, ErrNotFound
	} else if err != nil {
		return ConnectionDescriptor{}, err
	}
	row, err := getMachineForCapability(ctx, s.db.Queries(), dbsqlc.GetUserMachineForCapabilityParams{ID: userMachineID, UserID: userID, Capability: "terminal"})
	if errors.Is(err, sql.ErrNoRows) {
		return ConnectionDescriptor{}, ErrNotFound
	}
	if err != nil {
		return ConnectionDescriptor{}, err
	}
	if !slices.Contains(row.ConfiguredCapabilities, "terminal_host") {
		return ConnectionDescriptor{}, ErrMachineCapabilityUnavailable
	}
	terminalSession, err := s.terminalSession(ctx, userID, userMachineID, terminalSessionID)
	if err != nil {
		return ConnectionDescriptor{}, err
	}
	ttl := s.ttl
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if s.controlSigner != nil && ttl > 5*time.Minute {
		ttl = 5 * time.Minute
	}
	expires := s.now().UTC().Add(ttl)
	response := ConnectionDescriptor{Issuer: s.issuer, UserMachineID: row.ID, UserMachineState: row.State, ExpiresAt: expires, Status: "connector_connecting", Reason: "connector_offline", RetryAfterSeconds: 2}
	setCanonicalMachineIdentity(&response, row)
	if row.State == "revoked" || row.State == "disconnected" || row.State == "deleted" {
		response.Status = "user_machine_revoked"
		response.Reason = "access_revoked"
		return response, nil
	}
	if !row.Online || !slices.Contains(row.ObservedCapabilities, "terminal_host") {
		response.Status, response.Reason = "machine_offline", "terminal_host_unavailable"
		return response, nil
	}
	route, routeErr := s.db.Queries().GetActiveHelperRouteForEnvironment(ctx, row.EnvironmentID)
	if errors.Is(routeErr, sql.ErrNoRows) {
		return response, nil
	}
	if routeErr != nil {
		return ConnectionDescriptor{}, routeErr
	}
	httpIngressURL, ingressErr := s.runtimeIngressURL(ctx, row.EnvironmentID, route.PublicHost)
	if ingressErr != nil {
		return ConnectionDescriptor{}, ingressErr
	}
	httpBaseURL, websocketBaseURL := httpIngressURL, runtimeWebsocketBase(httpIngressURL)
	if cliClientSessionID == "" || s.controlSigner == nil && s.credentials == nil {
		return ConnectionDescriptor{}, errors.New("user-machine credential issuer is unavailable")
	}
	input := access.CredentialInput{UserID: userID, MachineID: row.ID, EnvironmentID: row.EnvironmentID, SourceMachineID: sourceMachineID, CLIClientSessionID: cliClientSessionID, HTTPBaseURL: httpBaseURL, ExpiresAt: expires}
	if s.controlSigner == nil {
		if err := s.credentials.CheckCLI(ctx, input); err != nil {
			return ConnectionDescriptor{}, err
		}
		if checker, ok := s.credentials.(interface {
			CheckHealth(context.Context, access.CredentialInput) error
		}); ok {
			if err := checker.CheckHealth(ctx, input); err != nil {
				response.Status = "connector_connecting"
				response.Reason = "helper_unhealthy"
				return response, nil
			}
		}
	}
	if err := s.applyTerminalSessionOperationsForSession(ctx, row.ID, terminalSession.ID); err != nil {
		response.Status = "helper_starting"
		response.Reason = "terminal_session_operation_pending"
		return response, nil
	}
	accessSessionID := newID("session")
	input.AccessSessionID = accessSessionID
	personal := row.UserID == userID && !row.OwnerTeamID.Valid
	if scope, selected := workspace.FromContext(ctx); selected {
		personal = personal && scope.TeamID == ""
	}
	includeFiles := personal && slices.Contains(row.ConfiguredCapabilities, "file_receive") && slices.Contains(row.ObservedCapabilities, "file_receive")
	credentials, credentialKind, err := s.issueUserMachineCredentials(ctx, input, terminalSession.ID, includeFiles)
	if err != nil {
		return ConnectionDescriptor{}, err
	}
	if len(compactSessionIDs(credentials.TerminalSessionID, credentials.FileSessionID)) == 0 {
		return ConnectionDescriptor{}, errors.New("user-machine credential issuer returned no revocable sessions")
	}
	if credentials.TerminalAuth == nil || includeFiles && credentials.FileTransferAuth == nil {
		return ConnectionDescriptor{}, errors.New("user-machine credential issuer returned incomplete authorization")
	}
	credentials.TerminalAuth["access_session_id"] = accessSessionID
	if credentials.FileTransferAuth != nil {
		credentials.FileTransferAuth["access_session_id"] = accessSessionID
	}
	capabilities := []string{"terminal"}
	if includeFiles {
		capabilities = append(capabilities, "files")
	}
	if personal && slices.Contains(row.ConfiguredCapabilities, "preview_launch") {
		capabilities = append(capabilities, "private_access")
	}
	if err := s.createAccessSession(ctx, dbsqlc.CreateUserMachineAccessSessionParams{
		Capabilities: capabilities, OperationID: terminalSession.ID, ID: accessSessionID, UserMachineID: row.ID, UserID: userID, EnvironmentID: row.EnvironmentID,
		CLIClientSessionID: cliClientSessionID, HttpBaseUrl: httpBaseURL,
		HelperTerminalSessionID: credentials.TerminalSessionID, HelperFileSessionID: credentials.FileSessionID,
		ExpiresAt: expires,
	}, credentialKind); err != nil {
		cleanupErr := s.revokeCredentialSessions(ctx, machineAccessSession{
			UserID: userID, UserMachineID: row.ID, EnvironmentID: row.EnvironmentID,
			CLIClientSessionID: cliClientSessionID, HTTPBaseURL: httpBaseURL,
			TerminalSessionID: credentials.TerminalSessionID, FileSessionID: credentials.FileSessionID, CredentialKind: credentialKind,
		}, "access_session_persistence_failed")
		return ConnectionDescriptor{}, errors.Join(err, cleanupErr)
	}
	response.Connectable, response.Status, response.Reason, response.RetryAfterSeconds = true, "ready", "ready", 0
	setCanonicalMachineIdentity(&response, row)
	response.Terminal = map[string]any{"protocol": "paperboat.terminal.v1", "endpoints": machineTerminalEndpoints(websocketBaseURL), "session_id": terminalSession.ID, "cwd": terminalSession.LaunchCwd, "auth": credentials.TerminalAuth}
	if credentials.FileTransferAuth != nil {
		response.FileTransfer = map[string]any{"endpoint": httpBaseURL + "/v1/file-transfers", "source_machine_id": sourceMachineID, "destination_machine_id": row.ID, "initiating_user_id": userID, "policy": s.fileTransferPolicy, "auth": credentials.FileTransferAuth}
	}
	return response, nil
}

func machineTerminalEndpoints(wss string) map[string]any {
	u, err := url.Parse(wss)
	if err != nil || u.Hostname() == "" {
		return map[string]any{}
	}
	host := u.Host
	if u.Port() == "" {
		host += ":443"
	}
	endpoint := "wss://" + u.Host + strings.TrimRight(u.Path, "/") + "/v1/runtime"
	if u.Path != "" {
		return map[string]any{"wss": endpoint}
	}
	return map[string]any{"quic": "quic://" + host, "wss": endpoint}
}

func (s *Service) issueUserMachineCredentials(ctx context.Context, input access.CredentialInput, terminalSessionID string, includeFiles bool) (access.CLICredentials, machineCredentialKind, error) {
	if s.controlSigner == nil {
		credentials, err := s.credentials.IssueCLI(ctx, input)
		return credentials, machineCredentialExternal, err
	}
	issuedAt := s.now().UTC()
	terminalJTI := newID("token")
	transferJTI := newID("token")
	sign := func(class string, scopes []string, jti string) (string, error) {
		return s.signProcessCredential(ctx, mint.CredentialInput{
			Issuer: s.issuer, Audience: "paperboat-machine", Subject: input.UserID, JTI: jti,
			IssuedAt: issuedAt, ExpiresAt: input.ExpiresAt, CredentialClass: class, Scopes: scopes,
			EnvironmentID: input.EnvironmentID, MachineID: input.MachineID, SourceMachineID: input.SourceMachineID, UserID: input.UserID, CLIClientSessionID: input.CLIClientSessionID, SessionID: terminalSessionID, AssignmentID: input.AccessSessionID,
		})
	}
	terminalToken, err := sign("terminal_operation", []string{"terminal:operate"}, terminalJTI)
	if err != nil {
		return access.CLICredentials{}, machineCredentialSigned, err
	}
	if !includeFiles {
		return access.CLICredentials{TerminalAuth: map[string]any{"method": "bearer", "token": terminalToken, "expires_at": input.ExpiresAt, "scopes": []string{"terminal:operate"}}, TerminalSessionID: terminalJTI}, machineCredentialSigned, nil
	}
	transferToken, err := sign("file_transfer", []string{"file:transfer"}, transferJTI)
	if err != nil {
		return access.CLICredentials{}, machineCredentialSigned, err
	}
	return access.CLICredentials{
		TerminalAuth:      map[string]any{"method": "bearer", "token": terminalToken, "expires_at": input.ExpiresAt, "scopes": []string{"terminal:operate"}},
		FileTransferAuth:  map[string]any{"method": "bearer", "token": transferToken, "expires_at": input.ExpiresAt, "scopes": []string{"file:transfer"}},
		TerminalSessionID: terminalJTI, FileSessionID: transferJTI,
	}, machineCredentialSigned, nil
}

func (s *Service) ConnectionReadiness(ctx context.Context, userID, userMachineID string) (ConnectionDescriptor, error) {
	return s.ConnectionReadinessForTerminalSession(ctx, userID, userMachineID, "")
}

func (s *Service) ConnectionReadinessForTerminalSession(ctx context.Context, userID, userMachineID, terminalSessionID string) (ConnectionDescriptor, error) {
	row, err := getMachineForCapability(ctx, s.db.Queries(), dbsqlc.GetUserMachineForCapabilityParams{ID: userMachineID, UserID: userID, Capability: "terminal"})
	if errors.Is(err, sql.ErrNoRows) {
		return ConnectionDescriptor{}, ErrNotFound
	}
	if err != nil {
		return ConnectionDescriptor{}, err
	}
	if _, err := s.terminalSession(ctx, userID, userMachineID, terminalSessionID); err != nil {
		return ConnectionDescriptor{}, err
	}
	response := ConnectionDescriptor{Issuer: s.issuer, UserMachineID: row.ID, UserMachineState: row.State, ExpiresAt: s.now().UTC(), Status: "connector_connecting", Reason: "connector_offline", RetryAfterSeconds: 2}
	setCanonicalMachineIdentity(&response, row)
	if row.State == "revoked" || row.State == "disconnected" || row.State == "deleted" || row.SeatState != "occupied" {
		response.Status = "user_machine_revoked"
		response.Reason = "access_revoked"
		return response, nil
	}
	if _, err := s.db.Queries().GetActiveHelperRouteForEnvironment(ctx, row.EnvironmentID); errors.Is(err, sql.ErrNoRows) {
		return response, nil
	} else if err != nil {
		return ConnectionDescriptor{}, err
	}
	response.Connectable, response.Status, response.Reason, response.RetryAfterSeconds = true, "ready", "ready", 0
	setCanonicalMachineIdentity(&response, row)
	return response, nil
}

func (s *Service) terminalSession(ctx context.Context, userID, userMachineID, sessionID string) (dbsqlc.UserMachineTerminalSession, error) {
	if strings.TrimSpace(sessionID) == "" {
		return dbsqlc.UserMachineTerminalSession{}, ErrTerminalSessionNotFound
	}
	row, err := getTerminalSession(ctx, s.db.Queries(), dbsqlc.GetUserMachineTerminalSessionParams{ID: sessionID, UserMachineID: userMachineID, UserID: userID})
	if errors.Is(err, sql.ErrNoRows) {
		return dbsqlc.UserMachineTerminalSession{}, ErrTerminalSessionNotFound
	}
	return row, err
}

func (s *Service) Approve(ctx context.Context, userID, userCode string) (UserMachine, error) {
	return s.approve(ctx, userID, userCode)
}

func (s *Service) approve(ctx context.Context, userID, userCode string) (UserMachine, error) {

	var out UserMachine
	var pairingID string
	var alreadyProvisioned bool
	var pairingExpired bool
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		pairing, err := tx.Queries().GetUserMachinePairingForCode(ctx, sql.NullString{String: strings.TrimSpace(userCode), Valid: true})
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if pairing.State == "approved" && pairing.ApprovedByUserID.Valid && pairing.ApprovedByUserID.String == userID && pairing.UserMachineID.Valid {
			row, err := tx.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: pairing.UserMachineID.String, UserID: userID})
			if err != nil {
				return err
			}
			if err := ensurePairingSSHTarget(ctx, tx, pairing, row); err != nil {
				return err
			}
			row, err = optInUserMachineKeepAwake(ctx, tx, userID, row, pairing.KeepAwake)
			if err != nil {
				return err
			}
			out, pairingID = mapMachine(row), pairing.ID
			alreadyProvisioned = len(pairing.InstallationConfigCiphertext) > 0
			return nil
		}
		if pairing.State != "pending" {
			return ErrPairingUsed
		}
		if !s.now().Before(pairing.ExpiresAt) {
			if _, err := tx.Queries().ExpireUserMachinePairing(ctx, pairing.ID); err != nil {
				return err
			}
			pairingExpired = true
			return nil
		}
		enrollment := pairing
		if pairing.BootstrapOwnerAccount.Valid && pairing.BootstrapOwnerAccount.String != userID || pairing.InstallationState != "awaiting_approval" {
			return ErrNotFound
		}
		if enrollment.UserMachineID.Valid {
			row, err := tx.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: enrollment.UserMachineID.String, UserID: userID})
			if err != nil || row.State == "revoked" || row.State == "deleted" || row.SeatState != "released" && row.SeatState != "occupied" || row.Platform != pairing.Platform.String || row.Architecture != pairing.Architecture.String || row.WorkspaceRoot != pairing.WorkspaceRoot.String || row.Alias != pairing.RequestedAlias.String {
				return ErrEnrollmentState
			}
			if row.SeatState == "released" {
				if s.seats == nil {
					return ErrSeatUnavailable
				}
				if err := s.seats.ReserveUserMachineSeat(ctx, tx, userID); err != nil {
					return err
				}
				if n, err := tx.Queries().OccupyUserMachineSeat(ctx, dbsqlc.OccupyUserMachineSeatParams{ID: row.ID, UserID: userID}); err != nil || n != 1 {
					if err != nil {
						return err
					}
					return ErrEnrollmentState
				}
				row.SeatState = "occupied"
			}

			// A retry may come from a preserved machine record after local runtime
			// identity loss. Bind the pairing's authenticated key before issuing
			// material so the returned installation generation is authoritative.
			row, err = tx.Queries().BindCanonicalMachineIdentity(ctx, dbsqlc.BindCanonicalMachineIdentityParams{
				EnvironmentID:     row.EnvironmentID,
				PublicIdentityKey: sql.NullString{String: pairing.PublicIdentityKey.String, Valid: true},
				Now:               s.now().UTC(),
			})
			if err != nil {
				return err
			}
			if n, err := tx.Queries().ApproveUserMachinePairing(ctx, dbsqlc.ApproveUserMachinePairingParams{UserID: sql.NullString{String: userID, Valid: true}, UserMachineID: sql.NullString{String: row.ID, Valid: true}, ID: pairing.ID}); err != nil || n != 1 {
				if err != nil {
					return err
				}
				return ErrPairingUsed
			}
			if err := s.ensureHelperRoute(ctx, tx, row.ID, row.EnvironmentID); err != nil {
				return err
			}
			if err := ensurePairingSSHTarget(ctx, tx, pairing, row); err != nil {
				return err
			}
			row, err = optInUserMachineKeepAwake(ctx, tx, userID, row, pairing.KeepAwake)
			if err != nil {
				return err
			}
			out, pairingID = mapMachine(row), pairing.ID
			return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "user_machine.installation_retried", ResourceType: "user_machine", ResourceID: row.ID, IdempotencyKey: "user_machine.installation_retried:" + pairing.ID, Metadata: map[string]any{"environment_id": row.EnvironmentID, "generation": enrollment.Generation}})
		}
		if pairing.BootstrapOwnerAccount.Valid {
			existing, identityErr := tx.Queries().GetUserMachineByPublicIdentityForUpdate(ctx, sql.NullString{String: pairing.PublicIdentityKey.String, Valid: true})
			if identityErr == nil {
				if existing.UserID != userID {
					return ErrMachineIdentityConflict
				}
				if existing.Platform != pairing.Platform.String || existing.Architecture != pairing.Architecture.String || existing.WorkspaceRoot != pairing.WorkspaceRoot.String || existing.State == "revoked" || existing.State == "deleted" {
					return ErrEnrollmentState
				}
				if existing.SeatState == "released" {
					if s.seats == nil {
						return ErrSeatUnavailable
					}
					if err := s.seats.ReserveUserMachineSeat(ctx, tx, userID); err != nil {
						return err
					}
					if n, err := tx.Queries().OccupyUserMachineSeat(ctx, dbsqlc.OccupyUserMachineSeatParams{ID: existing.ID, UserID: userID}); err != nil || n != 1 {
						if err != nil {
							return err
						}
						return ErrEnrollmentState
					}
				} else if existing.SeatState != "occupied" {
					return ErrEnrollmentState
				}
				row, err := tx.Queries().UpdateUserMachineInstallation(ctx, dbsqlc.UpdateUserMachineInstallationParams{ID: existing.ID, UserID: userID, Alias: existing.Alias, Description: pairing.RequestedDescription, WorkspaceRoot: pairing.WorkspaceRoot.String, RuntimeVersions: pairing.RuntimeVersions})
				if err != nil {
					return err
				}

				if err := s.ensureHelperRoute(ctx, tx, row.ID, row.EnvironmentID); err != nil {
					return err
				}
				if _, err := s.ensureCurrentBandwidthPeriod(ctx, tx, row); err != nil {
					return err
				}
				if n, err := tx.Queries().ApproveUserMachinePairing(ctx, dbsqlc.ApproveUserMachinePairingParams{UserID: sql.NullString{String: userID, Valid: true}, UserMachineID: sql.NullString{String: row.ID, Valid: true}, ID: pairing.ID}); err != nil || n != 1 {
					if err != nil {
						return err
					}
					return ErrPairingUsed
				}
				if err := ensurePairingSSHTarget(ctx, tx, pairing, row); err != nil {
					return err
				}
				row, err = optInUserMachineKeepAwake(ctx, tx, userID, row, pairing.KeepAwake)
				if err != nil {
					return err
				}
				out, pairingID = mapMachine(row), pairing.ID
				return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "user_machine.enrollment_restarted", ResourceType: "user_machine", ResourceID: row.ID, IdempotencyKey: "user_machine.enrollment_restarted:" + pairing.ID, Metadata: map[string]any{"environment_id": row.EnvironmentID, "generation": enrollment.Generation}})
			}
			if !errors.Is(identityErr, sql.ErrNoRows) {
				return identityErr
			}
			if s.seats == nil {
				return ErrSeatUnavailable
			}
			if err := s.seats.ReserveUserMachineSeat(ctx, tx, userID); err != nil {
				return err
			}
			machineID := newID("machine")
			alias, err := allocateFreshMachineAlias(ctx, tx.Queries(), userID, pairing.RequestedAlias.String)
			if err != nil {
				return err
			}
			row, err := tx.Queries().CreateEnrolledMachine(ctx, dbsqlc.CreateEnrolledMachineParams{
				ID: machineID, UserID: userID, EnvironmentID: machineID, Description: pairing.RequestedDescription, Alias: alias,
				Platform: pairing.Platform.String, Architecture: pairing.Architecture.String, WorkspaceRoot: pairing.WorkspaceRoot.String,
				RuntimeVersions: pairing.RuntimeVersions, ConfiguredCapabilities: defaultConfiguredCapabilities(),
				PublicIdentityKey: sql.NullString{String: pairing.PublicIdentityKey.String, Valid: true},
			})
			if userMachineTerminalSessionUniqueViolation(err) {
				return ErrMachineNameConflict
			}
			if err != nil {
				return err
			}
			if _, err := tx.Queries().CreateControlEnvironment(ctx, dbsqlc.CreateControlEnvironmentParams{ID: row.ID, WorkspaceID: row.ID, OwnerUserID: sql.NullString{String: userID, Valid: true}, DesiredState: "active"}); err != nil {
				return err
			}
			if n, err := tx.Queries().OccupyUserMachineSeat(ctx, dbsqlc.OccupyUserMachineSeatParams{ID: row.ID, UserID: userID}); err != nil || n != 1 {
				if err != nil {
					return err
				}
				return ErrEnrollmentState
			}
			row, err = tx.Queries().UpdateUserMachineInstallation(ctx, dbsqlc.UpdateUserMachineInstallationParams{ID: row.ID, UserID: userID, Alias: alias, Description: pairing.RequestedDescription, WorkspaceRoot: pairing.WorkspaceRoot.String, RuntimeVersions: pairing.RuntimeVersions})
			if err != nil {
				return err
			}

			if err := s.ensureHelperRoute(ctx, tx, row.ID, row.EnvironmentID); err != nil {
				return err
			}
			if _, err := s.ensureCurrentBandwidthPeriod(ctx, tx, row); err != nil {
				return err
			}
			if n, err := tx.Queries().ApproveUserMachinePairing(ctx, dbsqlc.ApproveUserMachinePairingParams{UserID: sql.NullString{String: userID, Valid: true}, UserMachineID: sql.NullString{String: row.ID, Valid: true}, ID: pairing.ID}); err != nil || n != 1 {
				if err != nil {
					return err
				}
				return ErrPairingUsed
			}
			if err := ensurePairingSSHTarget(ctx, tx, pairing, row); err != nil {
				return err
			}
			row, err = optInUserMachineKeepAwake(ctx, tx, userID, row, pairing.KeepAwake)
			if err != nil {
				return err
			}
			out, pairingID = mapMachine(row), pairing.ID
			return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "user_machine.enrollment_approved", ResourceType: "user_machine", ResourceID: row.ID, IdempotencyKey: "user_machine.enrollment_approved:" + pairing.ID, Metadata: map[string]any{"environment_id": row.EnvironmentID, "generation": enrollment.Generation}})
		}
		existing, identityErr := tx.Queries().GetUserMachineByPublicIdentityForUpdate(ctx, sql.NullString{String: pairing.PublicIdentityKey.String, Valid: true})
		if identityErr == nil {
			if existing.UserID != userID {
				return ErrMachineIdentityConflict
			}
			if existing.Platform != pairing.Platform.String || existing.Architecture != pairing.Architecture.String || existing.WorkspaceRoot != pairing.WorkspaceRoot.String {
				return ErrInvalidPairing
			}
			if existing.SeatState == "released" {
				if s.seats == nil {
					return ErrSeatUnavailable
				}
				if err := s.seats.ReserveUserMachineSeat(ctx, tx, userID); err != nil {
					return err
				}
				if n, err := tx.Queries().OccupyUserMachineSeat(ctx, dbsqlc.OccupyUserMachineSeatParams{ID: existing.ID, UserID: userID}); err != nil || n != 1 {
					if err != nil {
						return err
					}
					return ErrEnrollmentState
				}
			} else if existing.SeatState != "occupied" {
				return ErrEnrollmentState
			}
			row, err := tx.Queries().UpdateUserMachineInstallation(ctx, dbsqlc.UpdateUserMachineInstallationParams{
				ID: existing.ID, UserID: userID, Alias: pairing.RequestedAlias.String, Description: pairing.RequestedDescription,
				WorkspaceRoot: pairing.WorkspaceRoot.String, RuntimeVersions: pairing.RuntimeVersions,
			})
			if err != nil {
				return err
			}

			if err := s.ensureHelperRoute(ctx, tx, row.ID, row.EnvironmentID); err != nil {
				return err
			}
			if _, err := s.ensureCurrentBandwidthPeriod(ctx, tx, row); err != nil {
				return err
			}
			if n, err := tx.Queries().ApproveUserMachinePairing(ctx, dbsqlc.ApproveUserMachinePairingParams{UserID: sql.NullString{String: userID, Valid: true}, UserMachineID: sql.NullString{String: row.ID, Valid: true}, ID: pairing.ID}); err != nil || n != 1 {
				if err != nil {
					return err
				}
				return ErrPairingUsed
			}
			if err := ensurePairingSSHTarget(ctx, tx, pairing, row); err != nil {
				return err
			}
			row, err = optInUserMachineKeepAwake(ctx, tx, userID, row, pairing.KeepAwake)
			if err != nil {
				return err
			}
			out, pairingID = mapMachine(row), pairing.ID
			return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "machine.installation_resumed", ResourceType: "machine", ResourceID: row.ID, IdempotencyKey: "machine.installation_resumed:" + pairing.ID})
		}
		if !errors.Is(identityErr, sql.ErrNoRows) {
			return identityErr
		}
		return ErrInvalidPairing
	})
	if pairingExpired {
		return UserMachine{}, ErrPairingExpired
	}
	var aliasConflict *pgconn.PgError
	if errors.As(err, &aliasConflict) && aliasConflict.Code == "23505" && (aliasConflict.ConstraintName == "user_machines_owner_alias" || aliasConflict.ConstraintName == "user_machines_active_alias") {
		return UserMachine{}, ErrMachineNameConflict
	}

	if err != nil || alreadyProvisioned {
		return out, err
	}
	if s.helperGrant == nil {
		return UserMachine{}, ErrProvisioningUnavailable
	}
	if err := s.provisionApprovedUserMachine(ctx, userID, pairingID, out); err != nil {
		return UserMachine{}, err
	}
	return out, nil
}

func ensurePairingSSHTarget(ctx context.Context, tx *db.Tx, pairing dbsqlc.UserMachinePairing, machine dbsqlc.UserMachine) error {
	if !pairing.SshUser.Valid && !pairing.SshPort.Valid {
		return nil
	}
	if !pairing.SshUser.Valid || !pairing.SshPort.Valid || !validPairingSSHUser(pairing.SshUser.String) || pairing.SshPort.Int32 < 1 || pairing.SshPort.Int32 > 65535 || machine.InstallationGeneration < 1 {
		return ErrInvalidPairing
	}
	return tx.Queries().UpsertPairingMachineSSHTarget(ctx, dbsqlc.UpsertPairingMachineSSHTargetParams{
		UserMachineID: machine.ID, MachineGeneration: machine.InstallationGeneration,
		OsUser: pairing.SshUser.String, TargetPort: pairing.SshPort.Int32,
	})
}

func (s *Service) ensureHelperRoute(ctx context.Context, tx *db.Tx, userMachineID, environmentID string) error {
	if s.helperBaseDomain == "" || s.helperListenPort == 0 {
		return ErrProvisioningUnavailable
	}
	if _, err := tx.Queries().GetHelperRouteForEnvironment(ctx, environmentID); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.Queries().ReactivateHelperRouteForEnvironment(ctx, dbsqlc.ReactivateHelperRouteForEnvironmentParams{EnvironmentID: environmentID, Now: s.now().UTC()}); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	publicHost := strings.ReplaceAll(strings.ToLower(userMachineID), "_", "-") + "." + s.helperBaseDomain
	_, err := tx.Queries().CreateControlRoute(ctx, dbsqlc.CreateControlRouteParams{
		ID: newID("route"), EnvironmentID: environmentID, ConnectorID: "runtime", Kind: "runtime_https_wss",
		PublicHost: publicHost, TargetHost: "127.0.0.1", TargetPort: s.helperListenPort,
	})
	return err
}

func (s *Service) Deny(ctx context.Context, userID, userCode string) error {
	userID, userCode = strings.TrimSpace(userID), strings.TrimSpace(userCode)
	if userID == "" || userCode == "" {
		return ErrNotFound
	}
	var pairingExpired bool
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		pairing, err := tx.Queries().GetUserMachinePairingForCode(ctx, sql.NullString{String: userCode, Valid: true})
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if pairing.State != "pending" {
			return ErrPairingUsed
		}
		if !s.now().Before(pairing.ExpiresAt) {
			if _, err := tx.Queries().ExpireUserMachinePairing(ctx, pairing.ID); err != nil {
				return err
			}
			pairingExpired = true
			return nil
		}
		enrollment := pairing
		if pairing.BootstrapOwnerAccount.Valid && pairing.BootstrapOwnerAccount.String != userID || pairing.InstallationState != "awaiting_approval" {
			return ErrNotFound
		}
		if n, err := tx.Queries().DenyUserMachinePairing(ctx, dbsqlc.DenyUserMachinePairingParams{UserID: sql.NullString{String: userID, Valid: true}, ID: pairing.ID}); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return ErrPairingUsed
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "user_machine.denied", ResourceType: "pairing", ResourceID: enrollment.ID, IdempotencyKey: "user_machine.denied:" + pairing.ID})
	})
	if pairingExpired && err == nil {
		return ErrPairingExpired
	}
	return err
}

func (s *Service) provisionApprovedUserMachine(ctx context.Context, userID, pairingID string, machine UserMachine) error {
	return s.provisionApprovedUserMachineWithOperation(ctx, userID, pairingID, machine, "")
}

type installationMaterialBuild struct {
	plaintext, ciphertext                         []byte
	cliSessionID, cliAccessToken, cliRefreshToken string
	existingCLISessionID, refreshedAccessToken    string
	refreshedRefreshToken                         string
}

type installationClientSession struct {
	Schema       string `json:"schema"`
	SessionID    string `json:"session_id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

func (s *Service) persistInstallationCLISessionTx(ctx context.Context, tx *db.Tx, userID string, machine UserMachine, build installationMaterialBuild) error {
	if build.cliSessionID == "" {
		return nil
	}
	now := s.now().UTC()
	if err := auth.CreateClientSessionTx(ctx, tx, dbsqlc.CreateClientSessionParams{ID: build.cliSessionID, UserID: userID, ClientID: s.cliClientID, ClientLabel: "Paperboat enrollment: " + machine.Alias, DeviceType: "desktop", Os: machine.Platform, Scopes: s.cliScopes, CreatedAt: now, ApprovedAt: now}, build.cliAccessToken, build.cliRefreshToken, s.cliTokenPolicy()); err != nil {
		return err
	}
	if err := tx.Queries().MarkFreshE2EEBootstrapSession(ctx, build.cliSessionID); err != nil {
		return err
	}
	if bound, err := tx.Queries().BindCLIClientSessionToUserMachine(ctx, dbsqlc.BindCLIClientSessionToUserMachineParams{
		TargetMachineID: sql.NullString{String: machine.ID, Valid: true}, TargetCLISessionID: build.cliSessionID, TargetUserID: userID,
	}); err != nil {
		return err
	} else if bound != 1 {
		return ErrInstallationUnavailable
	}
	return nil
}

func (s *Service) buildInstallationMaterial(ctx context.Context, userID string, pairing dbsqlc.UserMachinePairing, machine UserMachine, operationKey string, existingClientSession json.RawMessage, reuseRuntimeIdentity bool, expectedHelperID string, exactArtifact *MachineArtifact, authorityGuard HelperEnrollmentAuthorityGuard) (installationMaterialBuild, error) {
	if strings.TrimSpace(s.encryptionKey) == "" {
		return installationMaterialBuild{}, errors.New("user-machine provisioning encryption is not configured")
	}
	if s.helperGrant == nil {
		return installationMaterialBuild{}, ErrProvisioningUnavailable
	}
	artifact, ok := s.machineArtifact(machine.Platform, machine.Architecture)
	if exactArtifact != nil {
		artifact, ok = *exactArtifact, *exactArtifact == artifact
	}
	if !ok {
		return installationMaterialBuild{}, errors.New("user-machine artifact is unavailable or changed")
	}
	installationEnrollmentID := pairing.ID
	operationKeyProvided := strings.TrimSpace(operationKey) != ""
	if !operationKeyProvided {
		operationKey = "machine-enrollment:" + pairing.ID
	}
	var grant HelperEnrollmentGrant
	var err error
	reuseIdentity := false
	if existing, existingErr := s.db.Queries().GetActiveControlHelperForEnvironment(ctx, machine.EnvironmentID); existingErr == nil {
		if reuseRuntimeIdentity {
			if expectedHelperID != "" && existing.ID != expectedHelperID {
				return installationMaterialBuild{}, ErrInstallationUnavailable
			}
			grant = HelperEnrollmentGrant{HelperID: existing.ID, ExpiresAt: s.now().UTC().Add(10 * time.Minute)}
			reuseIdentity = true
		} else {
			if s.helperRecovery == nil {
				return installationMaterialBuild{}, ErrProvisioningUnavailable
			}
			if !operationKeyProvided {
				operationKey = "machine-recovery:" + pairing.ID
			}
			if authorityGuard != nil {
				if s.authenticatedHelperRecovery == nil {
					return installationMaterialBuild{}, ErrProvisioningUnavailable
				}
				grant, err = s.authenticatedHelperRecovery(ctx, userID, operationKey, machine.EnvironmentID, existing.ID, 10*time.Minute, authorityGuard)
			} else {
				grant, err = s.helperRecovery(ctx, userID, operationKey, machine.EnvironmentID, existing.ID, 10*time.Minute)
			}
			if err != nil {
				return installationMaterialBuild{}, err
			}
		}
	} else if existingErr != nil && !errors.Is(existingErr, sql.ErrNoRows) {
		return installationMaterialBuild{}, existingErr
	} else if expectedHelperID != "" {
		// A journal that says the runtime was already enrolled must never be
		// silently rebound to a newly issued helper. The client will continue
		// using its persisted identity, so the original helper must still be
		// active for this recovery generation.
		return installationMaterialBuild{}, ErrInstallationUnavailable
	} else {
		if authorityGuard != nil {
			if s.authenticatedHelperGrant == nil {
				return installationMaterialBuild{}, ErrProvisioningUnavailable
			}
			grant, err = s.authenticatedHelperGrant(ctx, userID, operationKey, machine.EnvironmentID, 10*time.Minute, authorityGuard)
		} else {
			grant, err = s.helperGrant(ctx, userID, operationKey, machine.EnvironmentID, 10*time.Minute)
		}
		if err != nil {
			return installationMaterialBuild{}, err
		}
	}
	var cliSessionID, cliAccessToken, cliRefreshToken string
	if len(existingClientSession) == 0 && s.cliClientID != "" && s.cliScopes != "" && len(s.cliHashKey) > 0 && s.cliAccessLifetime > 0 && s.cliRefreshLifetime > 0 {
		cliSessionID = newID("session")
		cliAccessToken, cliRefreshToken = oneShotToken(32), oneShotToken(32)
	}
	materialMap := map[string]any{
		"schema": "paperboat.machine-installation/v1", "user_machine_id": machine.ID, "pairing_id": installationEnrollmentID, "environment_id": machine.EnvironmentID,
		"control_url": s.issuer, "helper_id": grant.HelperID, "enrollment_id": grant.EnrollmentID,
		"enrollment_credential": grant.Credential, "reuse_identity": reuseIdentity, "expires_at": grant.ExpiresAt,
		"artifact":                artifact,
		"helper_listen_address":   fmt.Sprintf("127.0.0.1:%d", s.helperListenPort),
		"installation_generation": machine.InstallationGeneration,
	}
	if len(existingClientSession) > 0 {
		var session any
		if err := json.Unmarshal(existingClientSession, &session); err != nil {
			return installationMaterialBuild{}, ErrInstallationUnavailable
		}
		materialMap["client_session"] = session
	}
	if cliSessionID != "" {
		materialMap["client_session"] = map[string]any{
			"schema": "paperboat.cli-session/v1", "session_id": cliSessionID,
			"access_token": cliAccessToken, "refresh_token": cliRefreshToken,
			"token_type": "Bearer", "expires_in": int(s.cliAccessLifetime / time.Second), "scope": s.cliScopes,
		}
	}
	material, err := json.Marshal(materialMap)
	if err != nil {
		return installationMaterialBuild{}, err
	}
	ciphertext, err := secrets.Encrypt(s.encryptionKey, string(material))
	if err != nil {
		return installationMaterialBuild{}, err
	}
	return installationMaterialBuild{plaintext: material, ciphertext: ciphertext, cliSessionID: cliSessionID, cliAccessToken: cliAccessToken, cliRefreshToken: cliRefreshToken}, nil
}

func (s *Service) provisionApprovedUserMachineWithOperation(ctx context.Context, userID, pairingID string, machine UserMachine, operationKey string) error {
	if strings.TrimSpace(s.encryptionKey) == "" {
		return errors.New("user-machine provisioning encryption is not configured")
	}
	if s.helperGrant == nil {
		return ErrProvisioningUnavailable
	}
	pairing, err := s.db.Queries().GetUserMachinePairingByID(ctx, pairingID)
	if err != nil {
		return err
	}
	build, err := s.buildInstallationMaterial(ctx, userID, pairing, machine, operationKey, nil, pairing.CanReuseRuntimeIdentity, "", nil, nil)
	if err != nil {
		return err
	}
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := s.persistInstallationCLISessionTx(ctx, tx, userID, machine, build); err != nil {
			return err
		}
		n, err := tx.Queries().SetUserMachineInstallationConfig(ctx, dbsqlc.SetUserMachineInstallationConfigParams{ID: pairingID, Ciphertext: build.ciphertext})
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrPairingUsed
		}
		return nil
	})
}

func (s *Service) provisionRecoveredUserMachine(ctx context.Context, userID string, pairing dbsqlc.UserMachinePairing, machine UserMachine, runtimeEnrolled bool) (json.RawMessage, error) {
	operationKey := pairing.InstallationRecoveryOperationKey.String
	if operationKey == "" {
		return nil, ErrInstallationUnavailable
	}
	var existingClientSession json.RawMessage
	var expectedHelperID string
	var exactArtifact *MachineArtifact
	var authenticatedBinding *authenticatedMachineInstallationMaterialBinding
	var materialSessionID string
	if len(pairing.InstallationConfigCiphertext) > 0 {
		plaintext, err := secrets.Decrypt(s.encryptionKey, pairing.InstallationConfigCiphertext)
		if err != nil {
			return nil, ErrInstallationUnavailable
		}
		existingHelperID, err := helperIDFromInstallationMaterial([]byte(plaintext))
		if err != nil {
			return nil, ErrInstallationUnavailable
		}
		if runtimeEnrolled {
			// Runtime-enrolled journals are allowed to reuse only the exact
			// helper identity that was persisted with the old material.
			// A different active helper would make the returned material fail
			// the client's binding check and could strand the local runtime.
			expectedHelperID = existingHelperID
		}
		existingClientSession, err = clientSessionFromInstallationMaterial([]byte(plaintext))
		if err != nil {
			return nil, ErrInstallationUnavailable
		}
		if len(existingClientSession) > 0 {
			var session installationClientSession
			if err := json.Unmarshal(existingClientSession, &session); err != nil || session.Schema != "paperboat.cli-session/v1" ||
				session.SessionID == "" || session.AccessToken == "" || session.RefreshToken == "" {
				return nil, ErrInstallationUnavailable
			}
			materialSessionID = session.SessionID
		}
		if pairing.AuthenticatedSetupCLISessionID.Valid {
			binding, err := authenticatedMachineInstallationMaterialBindingFromPlaintext([]byte(plaintext))
			if err != nil || !pairing.AuthenticatedSetupGeneration.Valid || binding.MachineID != machine.ID || binding.EnvironmentID != machine.EnvironmentID || binding.Generation != pairing.AuthenticatedSetupGeneration.Int64 {
				return nil, ErrInstallationUnavailable
			}
			authenticatedBinding = &binding
			exactArtifact = &binding.Artifact
		}
	}
	var authorityGuard HelperEnrollmentAuthorityGuard
	if authenticatedBinding != nil {
		authorityGuard = s.authenticatedMachineInstallationRecoveryAuthorityGuard(userID, pairing, *authenticatedBinding, materialSessionID)
	}
	build, err := s.buildInstallationMaterial(ctx, userID, pairing, machine, operationKey, existingClientSession, runtimeEnrolled, expectedHelperID, exactArtifact, authorityGuard)
	if err != nil {
		return nil, err
	}
	if len(existingClientSession) > 0 {
		var session installationClientSession
		if err := json.Unmarshal(existingClientSession, &session); err != nil || session.Schema != "paperboat.cli-session/v1" || session.SessionID == "" || session.AccessToken == "" || session.RefreshToken == "" || s.cliClientID == "" || len(s.cliHashKey) == 0 || s.cliAccessLifetime <= 0 || s.cliRefreshLifetime <= 0 {
			return nil, ErrInstallationUnavailable
		}
		session.AccessToken = oneShotToken(32)
		session.RefreshToken = oneShotToken(32)
		session.ExpiresIn = int(s.cliAccessLifetime / time.Second)
		updatedMaterial, err := replaceInstallationClientSession(build.plaintext, session)
		if err != nil {
			return nil, ErrInstallationUnavailable
		}
		build.plaintext = updatedMaterial
		build.ciphertext, err = secrets.Encrypt(s.encryptionKey, string(updatedMaterial))
		if err != nil {
			return nil, err
		}
		build.existingCLISessionID = session.SessionID
		build.refreshedAccessToken = session.AccessToken
		build.refreshedRefreshToken = session.RefreshToken
	}
	var completed int64
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if authorityGuard != nil {
			if guardErr := authorityGuard(ctx, tx, ""); guardErr != nil {
				return guardErr
			}
			current, currentErr := tx.Queries().GetUserMachinePairingForVerifier(ctx, pairing.VerifierHash)
			if currentErr != nil {
				return currentErr
			}
			if !s.authenticatedMachineInstallationMaterialMatches(build.ciphertext, machine.ID, machine.EnvironmentID, machine.InstallationGeneration, *exactArtifact, current.AuthenticatedSetupHelperEnrollmentID) {
				return ErrInstallationUnavailable
			}
		}
		if build.existingCLISessionID != "" {
			identity, identityErr := tx.Queries().GetClientSessionForInstallationRecovery(ctx, build.existingCLISessionID)
			if identityErr != nil || identity.UserID != userID || identity.ClientID != s.cliClientID {
				return ErrInstallationUnavailable
			}
			if bound, bindErr := tx.Queries().BindCLIClientSessionToUserMachine(ctx, dbsqlc.BindCLIClientSessionToUserMachineParams{
				TargetMachineID: sql.NullString{String: machine.ID, Valid: true}, TargetCLISessionID: build.existingCLISessionID, TargetUserID: userID,
			}); bindErr != nil {
				return bindErr
			} else if bound != 1 {
				return ErrInstallationUnavailable
			}
		}
		var completeErr error
		completed, completeErr = tx.Queries().CompleteUserMachineInstallationRecovery(ctx, dbsqlc.CompleteUserMachineInstallationRecoveryParams{
			Ciphertext: build.ciphertext, ExpiresAt: pairing.ExpiresAt, ID: pairing.ID, VerifierHash: pairing.VerifierHash,
			PublicIdentityKey: pairing.PublicIdentityKey, OperationKey: sql.NullString{String: operationKey, Valid: true},
		})
		if completeErr != nil {
			return completeErr
		}
		if completed != 1 {
			return nil
		}
		if build.existingCLISessionID != "" {
			now := s.now().UTC()
			if err := tx.Queries().RevokeClientAccessTokens(ctx, dbsqlc.RevokeClientAccessTokensParams{CLIClientSessionID: build.existingCLISessionID, RevokedAt: sql.NullTime{Time: now, Valid: true}}); err != nil {
				return err
			}
			if err := tx.Queries().RevokeClientRefreshTokens(ctx, dbsqlc.RevokeClientRefreshTokensParams{CLIClientSessionID: build.existingCLISessionID, RevokedAt: sql.NullTime{Time: now, Valid: true}}); err != nil {
				return err
			}
			if err := auth.IssueClientTokensTx(ctx, tx, build.existingCLISessionID, build.refreshedAccessToken, build.refreshedRefreshToken, now, s.cliTokenPolicy()); err != nil {
				return err
			}
		}
		if build.cliSessionID != "" {
			if err := s.persistInstallationCLISessionTx(ctx, tx, userID, machine, build); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if completed != 1 {
		// Another request completed the same recovery generation. Read its
		// ciphertext and return the winner rather than issuing another CLI
		// session or helper credential.
		ciphertext, replayErr := s.db.Queries().GetUserMachineInstallationConfigForReplay(ctx, dbsqlc.GetUserMachineInstallationConfigForReplayParams{VerifierHash: pairing.VerifierHash, PublicIdentityKey: pairing.PublicIdentityKey.String, Now: s.now().UTC()})
		if replayErr != nil {
			return nil, replayErr
		}
		plaintext, decryptErr := secrets.Decrypt(s.encryptionKey, ciphertext)
		if decryptErr != nil || !json.Valid([]byte(plaintext)) {
			return nil, ErrInstallationUnavailable
		}
		return json.RawMessage(plaintext), nil
	}
	return json.RawMessage(build.plaintext), nil
}

func clientSessionFromInstallationMaterial(material []byte) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(material, &fields); err != nil {
		return nil, err
	}
	session, ok := fields["client_session"]
	if !ok || string(session) == "null" || len(session) == 0 {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal(session, &value); err != nil {
		return nil, err
	}
	return session, nil
}

func helperIDFromInstallationMaterial(material []byte) (string, error) {
	var fields struct {
		HelperID string `json:"helper_id"`
	}
	if err := json.Unmarshal(material, &fields); err != nil || strings.TrimSpace(fields.HelperID) == "" {
		return "", ErrInstallationUnavailable
	}
	return fields.HelperID, nil
}

type authenticatedMachineInstallationMaterialBinding struct {
	MachineID     string          `json:"user_machine_id"`
	EnvironmentID string          `json:"environment_id"`
	HelperID      string          `json:"helper_id"`
	EnrollmentID  string          `json:"enrollment_id"`
	ReuseIdentity bool            `json:"reuse_identity"`
	Generation    int64           `json:"installation_generation"`
	Artifact      MachineArtifact `json:"artifact"`
}

func authenticatedMachineInstallationMaterialBindingFromPlaintext(material []byte) (authenticatedMachineInstallationMaterialBinding, error) {
	var binding authenticatedMachineInstallationMaterialBinding
	if err := json.Unmarshal(material, &binding); err != nil || binding.MachineID == "" || binding.EnvironmentID == "" || binding.HelperID == "" ||
		(!binding.ReuseIdentity && binding.EnrollmentID == "") || binding.Generation < 1 {
		return authenticatedMachineInstallationMaterialBinding{}, ErrInstallationUnavailable
	}
	return binding, nil
}

func (s *Service) authenticatedMachineInstallationMaterialMatches(ciphertext []byte, machineID, environmentID string, generation int64, artifact MachineArtifact, helperEnrollmentID sql.NullString) bool {
	plaintext, err := secrets.Decrypt(s.encryptionKey, ciphertext)
	if err != nil {
		return false
	}
	binding, err := authenticatedMachineInstallationMaterialBindingFromPlaintext([]byte(plaintext))
	if err != nil || binding.MachineID != machineID || binding.EnvironmentID != environmentID || binding.Generation != generation || binding.Artifact != artifact {
		return false
	}
	if binding.ReuseIdentity {
		return binding.EnrollmentID == ""
	}
	return helperEnrollmentID.Valid && binding.EnrollmentID == helperEnrollmentID.String
}

func (s *Service) authenticatedMachineInstallationRecoveryAuthorityGuard(userID string, expected dbsqlc.UserMachinePairing, binding authenticatedMachineInstallationMaterialBinding, materialSessionID string) HelperEnrollmentAuthorityGuard {
	return func(ctx context.Context, tx *db.Tx, helperEnrollmentID string) error {
		if !expected.AuthenticatedSetupCLISessionID.Valid || !expected.AuthenticatedSetupOperationID.Valid || !expected.AuthenticatedSetupGeneration.Valid ||
			!expected.InstallationRecoveryOperationKey.Valid ||
			expected.InstallationRecoveryOperationKey.String == "" {
			return ErrInstallationUnavailable
		}
		current, err := tx.Queries().GetUserMachinePairingForVerifier(ctx, expected.VerifierHash)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			return ErrInstallationUnavailable
		}
		row, err := tx.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: binding.MachineID, UserID: userID})
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			return ErrInstallationUnavailable
		}
		machine := mapMachine(row)
		artifact, artifactAvailable := s.machineArtifact(machine.Platform, machine.Architecture)
		if machine.EnvironmentID != binding.EnvironmentID || machine.SeatState != "occupied" ||
			machine.InstallationGeneration != binding.Generation || machine.PublicIdentityKey != expected.PublicIdentityKey.String || !artifactAvailable || artifact != binding.Artifact {
			return ErrInstallationUnavailable
		}
		if !authenticatedMachineInstallationRecoveryPairingMatches(current, expected, userID, machine, binding, s.now().UTC()) {
			return ErrInstallationUnavailable
		}
		originalSession, sessionErr := tx.Queries().GetActiveClientSessionForInstallation(ctx, expected.AuthenticatedSetupCLISessionID.String)
		if sessionErr != nil && !errors.Is(sessionErr, sql.ErrNoRows) {
			return sessionErr
		}
		if sessionErr != nil || originalSession.UserID != userID {
			if materialSessionID == "" || s.cliClientID == "" {
				return ErrInstallationUnavailable
			}
			materialSession, materialErr := tx.Queries().GetActiveClientSessionForUserMachine(ctx, dbsqlc.GetActiveClientSessionForUserMachineParams{
				ID: materialSessionID, UserID: userID, UserMachineID: sql.NullString{String: machine.ID, Valid: true},
			})
			if errors.Is(materialErr, sql.ErrNoRows) {
				return ErrInstallationUnavailable
			}
			if materialErr != nil {
				return materialErr
			}
			if materialSession.ClientID != s.cliClientID {
				return ErrInstallationUnavailable
			}
		}
		if helperEnrollmentID != "" {
			if n, bindErr := tx.Queries().BindAuthenticatedMachineInstallationHelperEnrollment(ctx, dbsqlc.BindAuthenticatedMachineInstallationHelperEnrollmentParams{ID: expected.ID, HelperEnrollmentID: sql.NullString{String: helperEnrollmentID, Valid: true}}); bindErr != nil {
				return bindErr
			} else if n != 1 {
				return ErrInstallationUnavailable
			}
		}
		return nil
	}
}

func authenticatedMachineInstallationRecoveryPairingMatches(current, expected dbsqlc.UserMachinePairing, userID string, machine UserMachine, binding authenticatedMachineInstallationMaterialBinding, now time.Time) bool {
	return current.ID == expected.ID && bytes.Equal(current.VerifierHash, expected.VerifierHash) && current.PublicIdentityKey == expected.PublicIdentityKey &&
		current.ApprovedByUserID.Valid && current.ApprovedByUserID.String == userID && current.UserMachineID.Valid && current.UserMachineID.String == machine.ID &&
		current.AuthenticatedSetupCLISessionID == expected.AuthenticatedSetupCLISessionID && current.AuthenticatedSetupOperationID == expected.AuthenticatedSetupOperationID &&
		current.AuthenticatedSetupGeneration.Valid && current.AuthenticatedSetupGeneration == expected.AuthenticatedSetupGeneration && current.AuthenticatedSetupGeneration.Int64 == binding.Generation &&

		current.RequestedAlias.String == machine.Alias && current.RequestedDescription == machine.Description && current.Platform.String == machine.Platform && current.Architecture.String == machine.Architecture &&
		current.WorkspaceRoot.String == machine.WorkspaceRoot && bytes.Equal(current.RuntimeVersions, machine.RuntimeVersions) &&
		current.SshUser == expected.SshUser && current.SshPort == expected.SshPort && current.CanReuseRuntimeIdentity == expected.CanReuseRuntimeIdentity &&
		current.State == "consumed" && current.InstallationConfigConsumedAt.Valid && current.InstallationConfigConsumedAt == expected.InstallationConfigConsumedAt &&
		current.InstallationRecoveryOperationKey.Valid && current.InstallationRecoveryOperationKey == expected.InstallationRecoveryOperationKey &&
		bytes.Equal(current.InstallationConfigCiphertext, expected.InstallationConfigCiphertext) && current.ExpiresAt.Equal(expected.ExpiresAt) && now.Before(current.ExpiresAt) &&
		binding.MachineID == machine.ID && binding.EnvironmentID == machine.EnvironmentID && binding.Artifact.Platform == machine.Platform && binding.Artifact.Architecture == machine.Architecture
}

func replaceInstallationClientSession(material []byte, session installationClientSession) ([]byte, error) {
	var fields map[string]any
	if err := json.Unmarshal(material, &fields); err != nil {
		return nil, err
	}
	fields["client_session"] = session
	return json.Marshal(fields)
}

func oneShotToken(size int) string {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// ReserveBandwidth atomically grants capacity from the machine's included
// period allowance and then the owner's paid top-ups. It intentionally grants
// a partial amount when the requested window crosses exhaustion; the caller
// must stop forwarding once that grant is consumed.
func (s *Service) ReserveBandwidth(ctx context.Context, userMachineID string, requestedBytes int64) (BandwidthReservation, error) {
	if strings.TrimSpace(userMachineID) == "" {
		return BandwidthReservation{}, ErrNotFound
	}
	if requestedBytes <= 0 {
		return BandwidthReservation{}, ErrInvalidBandwidth
	}
	var reservation BandwidthReservation
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		machine, err := tx.Queries().GetUserMachineForBandwidthUpdate(ctx, strings.TrimSpace(userMachineID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		reservation, err = s.reserveBandwidthForMachineTx(ctx, tx, machine, requestedBytes, s.now().UTC(), true)
		return err
	})
	return reservation, err
}

// DebitEnvironmentBandwidthTx reconciles trusted edge usage against a Machine
// environment inside the caller's transaction. Trusted reports are charged at
// the time their interval ended and remain chargeable after a machine disconnects
// or releases its seat.
func (s *Service) DebitEnvironmentBandwidthTx(ctx context.Context, tx *db.Tx, environmentID string, bytes int64, now time.Time) (int64, bool, error) {
	if tx == nil || strings.TrimSpace(environmentID) == "" || bytes <= 0 {
		return 0, false, ErrInvalidBandwidth
	}
	machine, err := tx.Queries().GetUserMachineForEnvironmentBandwidthUpdate(ctx, strings.TrimSpace(environmentID))
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	reservation, err := s.reserveBandwidthForMachineTx(ctx, tx, machine, bytes, now.UTC(), false)
	return reservation.GrantedBytes, reservation.Exhausted, err
}

func (s *Service) reserveBandwidthForMachineTx(ctx context.Context, tx *db.Tx, machine dbsqlc.UserMachine, requestedBytes int64, now time.Time, requireActiveMachine bool) (BandwidthReservation, error) {
	if requireActiveMachine && (machine.State != "online" || !slices.Contains(machine.ObservedCapabilities, "file_receive")) {
		return BandwidthReservation{}, ErrBandwidthDenied
	}
	period, err := s.ensureCurrentBandwidthPeriodAt(ctx, tx, machine, now)
	if err != nil {
		return BandwidthReservation{}, err
	}
	reservation := BandwidthReservation{}
	remaining := requestedBytes
	includedAvailable := period.IncludedBytes - period.ConsumedIncludedBytes
	if includedAvailable > 0 {
		consume := minInt64(remaining, includedAvailable)
		rows, err := tx.Queries().ConsumeUserMachineIncludedBandwidth(ctx, dbsqlc.ConsumeUserMachineIncludedBandwidthParams{UserMachineID: period.UserMachineID, PeriodStart: period.PeriodStart, Bytes: consume})
		if err != nil || rows != 1 {
			if err != nil {
				return BandwidthReservation{}, err
			}
			return BandwidthReservation{}, ErrBandwidthDenied
		}
		remaining -= consume
		reservation.GrantedBytes += consume
	}
	if remaining > 0 {
		topups, err := tx.Queries().ListActiveUserMachineTopupsForUpdate(ctx, machine.UserID)
		if err != nil {
			return BandwidthReservation{}, err
		}
		for _, topup := range topups {
			if remaining == 0 {
				break
			}
			consume := minInt64(remaining, topup.RemainingBytes)
			rows, err := tx.Queries().ConsumeUserMachineTopup(ctx, dbsqlc.ConsumeUserMachineTopupParams{ID: topup.ID, Bytes: consume})
			if err != nil || rows != 1 {
				if err != nil {
					return BandwidthReservation{}, err
				}
				return BandwidthReservation{}, ErrBandwidthDenied
			}
			remaining -= consume
			reservation.GrantedBytes += consume
		}
	}
	if reservation.GrantedBytes > 0 {
		topupBytes := reservation.GrantedBytes - minInt64(reservation.GrantedBytes, includedAvailable)
		rows, err := tx.Queries().RecordUserMachineTopupConsumption(ctx, dbsqlc.RecordUserMachineTopupConsumptionParams{UserMachineID: period.UserMachineID, PeriodStart: period.PeriodStart, Bytes: topupBytes})
		if err != nil || rows != 1 {
			if err != nil {
				return BandwidthReservation{}, err
			}
			return BandwidthReservation{}, ErrBandwidthDenied
		}
	}
	reservation.Exhausted = remaining > 0
	return reservation, nil
}

func (s *Service) ensureCurrentBandwidthPeriod(ctx context.Context, tx *db.Tx, machine dbsqlc.UserMachine) (dbsqlc.UserMachineBandwidthPeriod, error) {
	return s.ensureCurrentBandwidthPeriodAt(ctx, tx, machine, s.now().UTC())
}

func (s *Service) ensureCurrentBandwidthPeriodAt(ctx context.Context, tx *db.Tx, machine dbsqlc.UserMachine, now time.Time) (dbsqlc.UserMachineBandwidthPeriod, error) {
	entitlement, err := tx.Queries().GetUserMachineEntitlementForUpdate(ctx, machine.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		return dbsqlc.UserMachineBandwidthPeriod{}, ErrBandwidthDenied
	}
	if err != nil {
		return dbsqlc.UserMachineBandwidthPeriod{}, err
	}
	if (entitlement.State != "active" && entitlement.State != "trialing") || !now.Before(entitlement.CurrentPeriodEnd) || now.Before(entitlement.CurrentPeriodStart) {
		return dbsqlc.UserMachineBandwidthPeriod{}, ErrBandwidthDenied
	}
	return tx.Queries().UpsertUserMachineBandwidthPeriod(ctx, dbsqlc.UpsertUserMachineBandwidthPeriodParams{UserMachineID: machine.ID, PeriodStart: entitlement.CurrentPeriodStart, PeriodEnd: entitlement.CurrentPeriodEnd, IncludedBytes: entitlement.AllowanceBytes})
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func (s *Service) List(ctx context.Context, userID string, limit, offset int) ([]UserMachine, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	workspaceSet, workspaceTeamID := workspaceTeamArgument(ctx)
	var rows []dbsqlc.UserMachine
	var err error
	if workspaceSet {
		rows, err = s.db.Queries().ListUserMachinesInWorkspaceForUser(ctx, dbsqlc.ListUserMachinesInWorkspaceForUserParams{UserID: userID, WorkspaceSet: true, WorkspaceTeamID: workspaceTeamID, PageLimit: int32(limit), PageOffset: int32(offset)})
	} else {
		rows, err = s.db.Queries().ListUserMachinesForUser(ctx, dbsqlc.ListUserMachinesForUserParams{UserID: userID, PageLimit: int32(limit), PageOffset: int32(offset)})
	}
	if err != nil {
		return nil, 0, err
	}
	var total int32
	if workspaceSet {
		total, err = s.db.Queries().CountUserMachinesInWorkspaceForUser(ctx, dbsqlc.CountUserMachinesInWorkspaceForUserParams{UserID: userID, WorkspaceSet: true, WorkspaceTeamID: workspaceTeamID})
	} else {
		total, err = s.db.Queries().CountUserMachinesForUser(ctx, userID)
	}
	if err != nil {
		return nil, 0, err
	}
	out := make([]UserMachine, 0, len(rows))
	for _, row := range rows {
		out = append(out, mapMachine(row))
	}
	items := make([]*UserMachine, len(out))
	for i := range out {
		items[i] = &out[i]
	}
	if err := s.describeMachineAccess(ctx, userID, items...); err != nil {
		return nil, 0, err
	}
	return out, int(total), nil
}

func (s *Service) Get(ctx context.Context, userID, userMachineID string) (UserMachine, error) {
	workspaceSet, workspaceTeamID := workspaceTeamArgument(ctx)
	var row dbsqlc.UserMachine
	var err error
	if workspaceSet {
		row, err = s.db.Queries().GetVisibleUserMachineInWorkspace(ctx, dbsqlc.GetVisibleUserMachineInWorkspaceParams{ID: userMachineID, UserID: userID, WorkspaceSet: true, WorkspaceTeamID: workspaceTeamID})
	} else {
		row, err = s.db.Queries().GetVisibleUserMachine(ctx, dbsqlc.GetVisibleUserMachineParams{ID: userMachineID, UserID: userID})
	}
	if errors.Is(err, sql.ErrNoRows) {
		return UserMachine{}, ErrNotFound
	}
	if err != nil {
		return UserMachine{}, err
	}
	item := mapMachine(row)
	if err := s.describeMachineAccess(ctx, userID, &item); err != nil {
		return UserMachine{}, err
	}
	return item, nil
}

func (s *Service) Rename(ctx context.Context, userID, userMachineID, alias, description string) (UserMachine, error) {
	userID, userMachineID, alias = strings.TrimSpace(userID), strings.TrimSpace(userMachineID), strings.TrimSpace(alias)
	description = strings.TrimSpace(description)
	if userID == "" || userMachineID == "" || invalidMachineAlias(alias) || invalidMachineDescription(description) {
		return UserMachine{}, ErrInvalidMachineName
	}
	var result UserMachine
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := teams.Lock(ctx, tx); err != nil {
			return err
		}
		row, err := tx.Queries().RenameUserMachine(ctx, dbsqlc.RenameUserMachineParams{Alias: alias, Description: description, ID: userMachineID, UserID: userID})
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if userMachineTerminalSessionUniqueViolation(err) {
			return ErrMachineNameConflict
		}
		if err != nil {
			return err
		}
		result = mapMachine(row)
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "user_machine.renamed", ResourceType: "user_machine", ResourceID: userMachineID, IdempotencyKey: "user_machine.renamed:" + userMachineID + ":" + strconv.FormatInt(row.Version, 10), Metadata: map[string]any{"alias": alias, "version": row.Version}})
	})
	if err == nil {
		err = s.describeMachineAccess(ctx, userID, &result)
	}
	return result, err
}

func (s *Service) ListTerminalSessions(ctx context.Context, userID, userMachineID string) ([]TerminalSession, error) {
	machine, err := s.Get(ctx, userID, userMachineID)
	if err != nil {
		return nil, err
	}
	workspaceSet, workspaceTeamID := workspaceTeamArgument(ctx)
	var rows []dbsqlc.UserMachineTerminalSession
	if workspaceSet {
		rows, err = s.db.Queries().ListUserMachineTerminalSessionsInWorkspace(ctx, dbsqlc.ListUserMachineTerminalSessionsInWorkspaceParams{UserMachineID: userMachineID, UserID: userID, WorkspaceTeamID: workspaceTeamID})
	} else {
		rows, err = s.db.Queries().ListUserMachineTerminalSessions(ctx, dbsqlc.ListUserMachineTerminalSessionsParams{UserMachineID: userMachineID, UserID: userID})
	}
	if err != nil {
		return nil, err
	}
	out := make([]TerminalSession, 0, len(rows))
	for _, row := range rows {
		item := mapTerminalSession(row)
		item.Machine = cleanTerminalMetadata(machine.Alias, 128)
		if live, liveErr := s.TerminalSessionSharing(ctx, userID, item.ID); liveErr == nil {
			item.Title, item.StartedIn, item.State, item.LastActiveAt, item.ExpiresAt = live.Title, live.StartedIn, live.State, live.LastActiveAt, live.ExpiresAt
			item.CurrentDirectory, item.ForegroundProcess = live.CurrentDirectory, live.ForegroundProcess
			count := len(live.Participants)
			item.AttachedCount = &count
		}
		out = append(out, item)
	}
	return out, nil
}

func (s *Service) CreateTerminalSession(ctx context.Context, userID, userMachineID, name, idempotencyKey string, maxActive int) (TerminalSession, error) {
	return s.createTerminalSession(ctx, userID, "", userMachineID, name, idempotencyKey, "", maxActive)
}

func (s *Service) createTerminalSession(ctx context.Context, userID, sourceMachineID, userMachineID, name, idempotencyKey, cwd string, maxActive int) (TerminalSession, error) {
	if sourceMachineID != "" || cwd != "" {
		// An explicit local launch directory belongs to the authenticated owner
		// machine. A caller-supplied source ID never grants access to a target.
		if sourceMachineID == "" || cwd != "" && sourceMachineID != userMachineID {
			return TerminalSession{}, ErrNotFound
		}
		machine, err := s.db.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: sourceMachineID, UserID: userID})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return TerminalSession{}, ErrNotFound
			}
			return TerminalSession{}, err
		}
		if machine.RevokedAt.Valid || machine.State == "revoked" || machine.State == "deleted" {
			return TerminalSession{}, ErrNotFound
		}
		if cwd != "" {
			var valid bool
			cwd, valid = canonicalTerminalLaunchCWD(machine.Platform, cwd)
			if !valid {
				return TerminalSession{}, ErrTerminalSessionInvalidCWD
			}
		}
	}
	requestedName := strings.ToLower(strings.TrimSpace(name))
	if requestedName != "" && !validUserMachineTerminalSessionName(requestedName) {
		return TerminalSession{}, ErrTerminalSessionInvalidName
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		return TerminalSession{}, ErrTerminalSessionIdempotency
	}
	if maxActive <= 0 {
		return TerminalSession{}, ErrTerminalSessionLimit
	}
	if existing, err := getTerminalSessionByIdempotencyKey(ctx, s.db.Queries(), dbsqlc.GetUserMachineTerminalSessionByIdempotencyKeyParams{UserMachineID: userMachineID, UserID: userID, IdempotencyKey: sql.NullString{String: idempotencyKey, Valid: true}}); err == nil {
		return mapTerminalSession(existing), nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return TerminalSession{}, err
	}
	id := newID("terminal")
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := teams.Lock(ctx, tx); err != nil {
			return err
		}
		scope, scoped := workspace.FromContext(ctx)
		if scoped {
			if err := workspace.ValidateTx(ctx, tx, userID, scope); err != nil {
				return err
			}
		}
		_, err := lockTerminalSessions(ctx, tx.Queries(), dbsqlc.LockUserMachineTerminalSessionsParams{UserMachineID: userMachineID, UserID: userID})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		lockedMachine, err := getMachineForCapability(ctx, tx.Queries(), dbsqlc.GetUserMachineForCapabilityParams{ID: userMachineID, UserID: userID, Capability: "terminal"})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if cwd != "" && (lockedMachine.UserID != userID || lockedMachine.OwnerTeamID.Valid || lockedMachine.RevokedAt.Valid) {
			return ErrNotFound
		}
		if err := terminalHostAvailability(lockedMachine); err != nil {
			return err
		}
		if existing, err := getTerminalSessionByIdempotencyKey(ctx, tx.Queries(), dbsqlc.GetUserMachineTerminalSessionByIdempotencyKeyParams{UserMachineID: userMachineID, UserID: userID, IdempotencyKey: sql.NullString{String: idempotencyKey, Valid: true}}); err == nil {
			id = existing.ID
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		count, err := tx.Queries().CountLiveUserMachineTerminalSessions(ctx, userMachineID)
		if err != nil {
			return err
		}
		if int(count) >= maxActive {
			return &TerminalSessionCapacityError{LiveSessions: int(count), Limit: maxActive}
		}
		sessionName := requestedName
		for attempts := 0; attempts < 32; attempts++ {
			if requestedName == "" {
				sessionName, err = naming.Session(s.nameRandom)
				if err != nil {
					return err
				}
				// teams.Lock serializes admission across machines. Skip every
				// retained handle for this owner, including closed sessions.
				var used bool
				if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM user_machine_terminal_sessions WHERE owner_account=$1 AND lower(name)=lower($2))`, userID, sessionName).Scan(&used); err != nil {
					return err
				}
				if used {
					continue
				}
			}
			ownerTeamID := sql.NullString{}
			if scoped && scope.TeamID != "" {
				ownerTeamID = sql.NullString{String: scope.TeamID, Valid: true}
			} else if !scoped && lockedMachine.OwnerTeamID.Valid {
				ownerTeamID = lockedMachine.OwnerTeamID
			}
			launchCWD := lockedMachine.WorkspaceRoot
			if cwd != "" {
				launchCWD = cwd
			}
			created, createErr := tx.Queries().CreateUserMachineTerminalSession(ctx, dbsqlc.CreateUserMachineTerminalSessionParams{OwnerAccount: userID, OwnerTeamID: ownerTeamID, ID: id, UserMachineID: userMachineID, Name: sessionName, IdempotencyKey: sql.NullString{String: idempotencyKey, Valid: true}, LaunchCwd: launchCWD})
			if createErr != nil {
				return createErr
			}
			if created > 0 {
				return tx.Queries().QueueUserMachineTerminalSessionOperation(ctx, dbsqlc.QueueUserMachineTerminalSessionOperationParams{ID: newID("operation"), UserMachineID: userMachineID, TerminalSessionID: id, Operation: "create"})
			}
			if requestedName != "" {
				return ErrTerminalSessionConflict
			}
		}
		return ErrTerminalSessionConflict
	})
	if err != nil {
		if userMachineTerminalSessionUniqueViolation(err) {
			existing, lookupErr := getTerminalSessionByIdempotencyKey(ctx, s.db.Queries(), dbsqlc.GetUserMachineTerminalSessionByIdempotencyKeyParams{UserMachineID: userMachineID, UserID: userID, IdempotencyKey: sql.NullString{String: idempotencyKey, Valid: true}})
			if lookupErr == nil {
				return mapTerminalSession(existing), nil
			}
			if !errors.Is(lookupErr, sql.ErrNoRows) {
				return TerminalSession{}, lookupErr
			}
			return TerminalSession{}, ErrTerminalSessionConflict
		}
		return TerminalSession{}, err
	}
	row, err := getTerminalSession(ctx, s.db.Queries(), dbsqlc.GetUserMachineTerminalSessionParams{ID: id, UserMachineID: userMachineID, UserID: userID})
	if err != nil {
		return TerminalSession{}, err
	}
	created := mapTerminalSession(row)
	return created, nil
}

func terminalHostAvailability(machine dbsqlc.UserMachine) error {
	if !slices.Contains(machine.ConfiguredCapabilities, "terminal_host") {
		return ErrMachineCapabilityUnavailable
	}
	if !machine.Online || !slices.Contains(machine.ObservedCapabilities, "terminal_host") {
		return ErrMachineOffline
	}
	return nil
}

func userMachineTerminalSessionUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (s *Service) CreateConfiguredTerminalSession(ctx context.Context, userID, userMachineID, name, idempotencyKey string) (TerminalSession, error) {
	return s.CreateTerminalSession(ctx, userID, userMachineID, name, idempotencyKey, s.maxSessions)
}

// CreateConfiguredTerminalSessionInDirectory creates an owner-machine session at
// the invoking directory. Replays retain the original session and launch directory.
func (s *Service) CreateConfiguredTerminalSessionInDirectory(ctx context.Context, userID, sourceMachineID, userMachineID, name, idempotencyKey, cwd string) (TerminalSession, error) {
	if sourceMachineID == "" {
		return TerminalSession{}, ErrNotFound
	}
	return s.createTerminalSession(ctx, userID, sourceMachineID, userMachineID, name, idempotencyKey, cwd, s.maxSessions)
}

func canonicalTerminalLaunchCWD(platform, cwd string) (string, bool) {
	if cwd == "" || len(cwd) > 1024 || !utf8.ValidString(cwd) || strings.ContainsRune(cwd, '\x00') {
		return "", false
	}
	if strings.EqualFold(platform, "windows") {
		return canonicalWorkspaceRoot(platform, cwd)
	}
	// POSIX syntax must not depend on the control plane operating system.
	return cwd, path.IsAbs(cwd) && path.Clean(cwd) == cwd
}

func validUserMachineTerminalSessionName(name string) bool {
	return name != "default" && terminalSessionNamePattern.MatchString(name)
}

func (s *Service) RenameTerminalSession(ctx context.Context, userID, userMachineID, id, name string) (TerminalSession, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !validUserMachineTerminalSessionName(name) {
		return TerminalSession{}, ErrTerminalSessionInvalidName
	}
	var result TerminalSession
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := teams.Lock(ctx, tx); err != nil {
			return err
		}
		workspaceSet, workspaceTeamID := workspaceTeamArgument(ctx)
		if workspaceSet {
			scope, _ := workspace.FromContext(ctx)
			if err := workspace.ValidateTx(ctx, tx, userID, scope); err != nil {
				return err
			}
		}
		var n int64
		var err error
		if workspaceSet {
			n, err = tx.Queries().RenameUserMachineTerminalSessionInWorkspace(ctx, dbsqlc.RenameUserMachineTerminalSessionInWorkspaceParams{ID: id, UserMachineID: userMachineID, OwnerAccount: userID, Name: name, WorkspaceTeamID: workspaceTeamID})
		} else {
			n, err = tx.Queries().RenameUserMachineTerminalSession(ctx, dbsqlc.RenameUserMachineTerminalSessionParams{ID: id, UserMachineID: userMachineID, OwnerAccount: userID, Name: name})
		}
		if userMachineTerminalSessionUniqueViolation(err) {
			return ErrTerminalSessionConflict
		}
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrTerminalSessionNotFound
		}
		row, err := getTerminalSession(ctx, tx.Queries(), dbsqlc.GetUserMachineTerminalSessionParams{ID: id, UserMachineID: userMachineID, UserID: userID})
		if err != nil {
			return err
		}
		result = mapTerminalSession(row)
		return nil
	})
	return result, err
}

func cleanTerminalMetadata(value string, maximum int) string {
	value = strings.TrimSpace(value)
	out := make([]rune, 0, minInt(utf8.RuneCountInString(value), maximum))
	for _, r := range value {
		if !unicode.IsControl(r) && !unicode.In(r, unicode.Bidi_Control) && len(out) < maximum {
			out = append(out, r)
		}
	}
	return string(out)
}

// CloseTerminalSession queues a signed Helper control operation. It returns
// false when the operation is durable but the connector is offline, allowing
// the HTTP handler to report an accepted/pending result instead of discarding
// the user's request.
func (s *Service) CloseTerminalSession(ctx context.Context, userID, userMachineID, id string) (bool, error) {
	return s.transitionTerminalSession(ctx, userID, userMachineID, id, false)
}
func (s *Service) DeleteTerminalSession(ctx context.Context, userID, userMachineID, id string) (bool, error) {
	return s.transitionTerminalSession(ctx, userID, userMachineID, id, true)
}
func (s *Service) transitionTerminalSession(ctx context.Context, userID, machine, id string, remove bool) (bool, error) {
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := teams.Lock(ctx, tx); err != nil {
			return err
		}
		scope, scoped := workspace.FromContext(ctx)
		workspaceSet, workspaceTeamID := workspaceTeamArgument(ctx)
		if scoped {
			if err := workspace.ValidateTx(ctx, tx, userID, scope); err != nil {
				return err
			}
		}
		row, err := getTerminalSession(ctx, tx.Queries(), dbsqlc.GetUserMachineTerminalSessionParams{ID: id, UserMachineID: machine, UserID: userID})
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTerminalSessionNotFound
		}
		if err != nil {
			return err
		}
		operation := "close"
		var n int64
		if remove {
			if row.IsDefault {
				return ErrTerminalSessionReserved
			}
			if !terminalSessionRuntimeClosed(row.RuntimeState) {
				return ErrTerminalSessionNotClosed
			}
			operation = "delete_history"
			if workspaceSet {
				n, err = tx.Queries().DeleteUserMachineTerminalSessionInWorkspace(ctx, dbsqlc.DeleteUserMachineTerminalSessionInWorkspaceParams{UserMachineID: machine, ID: id, WorkspaceTeamID: workspaceTeamID})
			} else {
				n, err = tx.Queries().DeleteUserMachineTerminalSession(ctx, dbsqlc.DeleteUserMachineTerminalSessionParams{UserMachineID: machine, ID: id})
			}
		} else {
			if workspaceSet {
				n, err = tx.Queries().CloseUserMachineTerminalSessionInWorkspace(ctx, dbsqlc.CloseUserMachineTerminalSessionInWorkspaceParams{UserMachineID: machine, ID: id, WorkspaceTeamID: workspaceTeamID})
			} else {
				n, err = tx.Queries().CloseUserMachineTerminalSession(ctx, dbsqlc.CloseUserMachineTerminalSessionParams{UserMachineID: machine, ID: id})
			}
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		return tx.Queries().QueueUserMachineTerminalSessionOperation(ctx, dbsqlc.QueueUserMachineTerminalSessionOperationParams{ID: newID("operation"), UserMachineID: machine, TerminalSessionID: id, Operation: operation})
	})
	if err != nil {
		return false, err
	}
	if err = s.applyTerminalSessionOperationsForSession(ctx, machine, id); err != nil {
		return false, nil
	}
	return true, nil
}

func (s *Service) ApplyTerminalSessionOperations(ctx context.Context, userMachineID string) error {
	return s.applyTerminalSessionOperations(ctx, userMachineID, "")
}

func (s *Service) applyTerminalSessionOperationsForSession(ctx context.Context, userMachineID, terminalSessionID string) error {
	return s.applyTerminalSessionOperations(ctx, userMachineID, terminalSessionID)
}

func (s *Service) applyTerminalSessionOperations(ctx context.Context, userMachineID, terminalSessionID string) error {
	for {
		items, err := s.db.Queries().ListPendingUserMachineTerminalSessionOperations(ctx, dbsqlc.ListPendingUserMachineTerminalSessionOperationsParams{UserMachineID: userMachineID, BatchSize: 32})
		if err != nil {
			return err
		}
		if len(items) == 0 {
			return nil
		}
		for _, item := range items {
			if terminalSessionID != "" && item.TerminalSessionID != terminalSessionID {
				continue
			}
			if err := s.applyTerminalSessionOperation(ctx, item.ID, item.UserMachineID, item.TerminalSessionID, item.Operation, item.Attempts, item.UserID, item.EnvironmentID, item.ProviderRouteHttpBaseUrl); err != nil {
				return err
			}
		}
		if terminalSessionID != "" {
			return nil
		}
	}
}

func (s *Service) processDueTerminalSessionOperations(ctx context.Context) error {
	items, err := s.db.Queries().ListDueUserMachineTerminalSessionOperations(ctx, 32)
	if err != nil {
		return err
	}
	var errs []error
	for _, item := range items {
		if err := s.applyTerminalSessionOperation(ctx, item.ID, item.UserMachineID, item.TerminalSessionID, item.Operation, item.Attempts, item.UserID, item.EnvironmentID, item.ProviderRouteHttpBaseUrl); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Service) expireClosedTerminalSessions(ctx context.Context) error {
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		expired, err := tx.Queries().ExpireClosedUserMachineTerminalSessions(ctx, dbsqlc.ExpireClosedUserMachineTerminalSessionsParams{
			ExpiredBefore: sql.NullTime{Time: s.now().UTC().Add(-terminalSessionClosedRecordRetention), Valid: true},
			BatchSize:     32,
		})
		if err != nil {
			return err
		}
		for _, item := range expired {
			if err := tx.Queries().QueueUserMachineTerminalSessionOperation(ctx, dbsqlc.QueueUserMachineTerminalSessionOperationParams{
				ID: newID("operation"), UserMachineID: item.UserMachineID, TerminalSessionID: item.ID, Operation: "delete_history",
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) applyTerminalSessionOperation(ctx context.Context, operationID, userMachineID, terminalSessionID, operation string, attempts int32, userID, environmentID, route string) error {
	if s.controlSigner == nil || s.controlRuntime == nil || strings.TrimSpace(route) == "" || strings.TrimSpace(s.issuer) == "" {
		return s.retryTerminalSessionOperation(ctx, operationID, attempts, errors.New("user-machine terminal control is unavailable"))
	}
	now := s.now().UTC()
	credential, err := s.signProcessCredential(ctx, mint.CredentialInput{
		Issuer: s.issuer, Audience: "paperboat-machine", Subject: userID, JTI: newID("token"),
		IssuedAt: now, ExpiresAt: now.Add(mint.MaxProofTTL), CredentialClass: "terminal_operation", Scopes: []string{"terminal:operate"},
		EnvironmentID: environmentID, MachineID: userMachineID, UserID: userID, CLIClientSessionID: operationID, SessionID: terminalSessionID,
	})
	if err == nil {
		action := operation
		if action == "delete_history" {
			action = "delete"
		}
		var observed helperruntime.Snapshot
		if action == "create" {
			row, lookupErr := s.db.Queries().GetUserMachineTerminalSession(ctx, dbsqlc.GetUserMachineTerminalSessionParams{ID: terminalSessionID, UserMachineID: userMachineID, UserID: userID})
			if lookupErr != nil {
				err = lookupErr
			} else if creator, ok := s.controlRuntime.(interface {
				CreateTerminal(context.Context, string, string, string, string, string, string) (helperruntime.Snapshot, error)
			}); !ok {
				err = errors.New("user-machine terminal creation is unavailable")
			} else {
				observed, err = creator.CreateTerminal(ctx, route, credential, terminalSessionID, operationID, row.Name, row.LaunchCwd)
				if err == nil && observed.Generation < 1 {
					err = errors.New("host did not start terminal process")
				}
				if err == nil {
					_, err = s.db.SQL().ExecContext(ctx, `UPDATE user_machine_terminal_sessions SET runtime_state=$2,last_runtime_sync_at=now(),last_runtime_sequence=$3,updated_at=now() WHERE id=$1`, terminalSessionID, observed.State, int64(observed.LatestSequence))
				}
			}
		} else {
			observed, err = s.controlRuntime.Terminal(ctx, route, credential, action, terminalSessionID, operationID)
		}
		if err == nil && action == "close" && observed.State != "closed" {
			err = fmt.Errorf("helper runtime acknowledged close in state %q", observed.State)
		}
		if err == nil && action == "close" {
			err = s.db.Queries().MarkUserMachineTerminalSessionRuntimeClosed(ctx, terminalSessionID)
		}
	}
	if err != nil {
		return s.retryTerminalSessionOperation(ctx, operationID, attempts, err)
	}
	return s.db.Queries().MarkUserMachineTerminalSessionOperationApplied(ctx, operationID)
}

func (s *Service) retryTerminalSessionOperation(ctx context.Context, id string, attempts int32, cause error) error {
	multiplier := 1 << minInt(8, int(attempts))
	backoff := multiplier
	if backoff > 300 {
		backoff = 300
	}
	err := s.db.Queries().RetryUserMachineTerminalSessionOperation(ctx, dbsqlc.RetryUserMachineTerminalSessionOperationParams{ID: id, RetrySeconds: float64(backoff), LastError: sql.NullString{String: terminalErrorSummary(ctx, cause), Valid: true}})
	return errors.Join(cause, err)
}

func terminalErrorSummary(ctx context.Context, err error) string {
	fault := supportreport.ProjectFault(ctx, "paperboat-server", "worker_machines", "terminal_session_operation", "terminal_operation_failed", err)
	parts := []string{"cause=" + fault.Cause, "error_type=" + fault.ErrorType}
	var protocol interface{ DiagnosticProtocolCode() string }
	if errors.As(err, &protocol) {
		parts = append(parts, "protocol_code="+protocol.DiagnosticProtocolCode())
	}
	if fault.Errno > 0 {
		parts = append(parts, "errno="+strconv.Itoa(fault.Errno))
	}
	if len(fault.ErrorChain) > 0 {
		chain := fault.ErrorChain
		if len(chain) > 2 {
			chain = chain[:2]
		}
		parts = append(parts, "error_chain="+strings.Join(chain, ","))
	}
	return strings.Join(parts, " ")
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func mapTerminalSession(row dbsqlc.UserMachineTerminalSession) TerminalSession {
	session := TerminalSession{ID: row.ID, Name: row.Name, IsDefault: row.IsDefault, State: "unavailable", StartedIn: cleanTerminalMetadata(row.LaunchCwd, 1024), CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	if !row.IsDefault {
		session.ExpiresAt = terminalSessionExpiresAt(row.RuntimeState, row.ClosedAt)
	}
	session.State = terminalDisplayState(row.DesiredState, row.RuntimeState, false, 0)
	if row.LastActivityAt.Valid {
		value := row.LastActivityAt.Time
		session.LastActiveAt = &value
	}
	return session
}

func terminalSessionRuntimeClosed(state string) bool {
	return state == "closed" || state == "exited" || state == "deleted"
}

func terminalSessionExpiresAt(runtimeState string, closedAt sql.NullTime) *time.Time {
	if !terminalSessionRuntimeClosed(runtimeState) || !closedAt.Valid {
		return nil
	}
	expiresAt := closedAt.Time.UTC().Add(terminalSessionClosedRecordRetention)
	return &expiresAt
}

// Disconnect explicitly revokes the local enrollment and releases its seat.
// Offline status is intentionally not treated as disconnect.
func (s *Service) Disconnect(ctx context.Context, userID, userMachineID string) error {
	if err := s.revokeUserMachineControl(ctx, userID, userMachineID, false); err != nil {
		return err
	}
	auditErr := s.audit.Write(ctx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "user_machine.disconnected", ResourceType: "user_machine", ResourceID: userMachineID, IdempotencyKey: "user_machine.disconnected:" + userMachineID, Metadata: map[string]any{}})
	return errors.Join(auditErr, s.RevokeUserMachineSessions(ctx, userMachineID, "user_machine_disconnected"))
}

func expireAuthenticatedMachineInstallationPairingsTx(ctx context.Context, tx *db.Tx, machineID string, now time.Time) error {
	_, err := tx.Queries().ExpireAuthenticatedMachineInstallationPairingsForMachine(ctx, dbsqlc.ExpireAuthenticatedMachineInstallationPairingsForMachineParams{Now: now, UserMachineID: sql.NullString{String: machineID, Valid: true}})
	return err
}

func (s *Service) Delete(ctx context.Context, userID, userMachineID string) error {
	if err := s.revokeUserMachineControl(ctx, userID, userMachineID, true); err != nil {
		return err
	}
	auditErr := s.audit.Write(ctx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "user_machine.deleted", ResourceType: "user_machine", ResourceID: userMachineID, IdempotencyKey: "user_machine.deleted:" + userMachineID, Metadata: map[string]any{}})
	return errors.Join(auditErr, s.RevokeUserMachineSessions(ctx, userMachineID, "user_machine_deleted"))
}

func (s *Service) revokeUserMachineControl(ctx context.Context, userID, userMachineID string, deleted bool) error {
	now := s.now().UTC()
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := environment.LockVaultMutationsTx(ctx, tx); err != nil {
			return err
		}
		machine, err := tx.Queries().GetUserMachineForUpdate(ctx, dbsqlc.GetUserMachineForUpdateParams{ID: userMachineID, UserID: userID})
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var changed int64
		if deleted {
			changed, err = tx.Queries().DeleteUserMachine(ctx, dbsqlc.DeleteUserMachineParams{ID: userMachineID, UserID: userID})
		} else {
			changed, err = tx.Queries().RevokeUserMachine(ctx, dbsqlc.RevokeUserMachineParams{ID: userMachineID, UserID: userID, State: "disconnected", SeatState: "released"})
		}
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrNotFound
		}
		if machine.State != "disconnected" && machine.State != "deleted" && machine.State != "revoked" {
			if err := db.FenceRemovedMachineVaultTx(ctx, tx, machine.UserID); err != nil {
				return err
			}
		}
		if deleted {
			if err := db.CleanupMachineMachineTx(ctx, tx, machine.UserID, machine.ID, now); err != nil {
				return err
			}
		}
		if err := expireAuthenticatedMachineInstallationPairingsTx(ctx, tx, machine.ID, now); err != nil {
			return err
		}
		return db.RevokeMachineEnvironmentTx(ctx, tx, machine.EnvironmentID, now)
	})
}

// RevokeUserMachineSessions records revocation before attempting the downstream
// call. Failed calls remain pending for Worker so revocation is eventually
// propagated without keeping the user's disconnect action hostage to an
// offline connector.
func (s *Service) RevokeUserMachineSessions(ctx context.Context, userMachineID, reason string) error {
	if strings.TrimSpace(userMachineID) == "" || strings.TrimSpace(reason) == "" {
		return errors.New("user-machine revocation input is incomplete")
	}
	rows, err := s.db.Queries().RevokeUserMachineAccessSessions(ctx, dbsqlc.RevokeUserMachineAccessSessionsParams{
		UserMachineID: userMachineID, Reason: sql.NullString{String: reason, Valid: true},
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, row := range rows {
		if err := s.revokeCredentialSessions(ctx, machineAccessSession{ID: row.ID, UserID: row.UserID, UserMachineID: row.UserMachineID, EnvironmentID: row.EnvironmentID, CLIClientSessionID: row.CLIClientSessionID, HTTPBaseURL: row.HttpBaseUrl, TerminalSessionID: row.HelperTerminalSessionID, FileSessionID: row.HelperFileSessionID}, reason); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := s.db.Queries().MarkUserMachineAccessSessionRevoked(ctx, row.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RevokeUserSessions is called after an entitlement revocation. It has the
// same durable retry behavior as an explicit machine disconnect.
func (s *Service) RevokeUserSessions(ctx context.Context, userID, reason string) error {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(reason) == "" {
		return errors.New("user-machine user revocation input is incomplete")
	}
	rows, err := s.db.Queries().RevokeUserMachineAccessSessionsForUser(ctx, dbsqlc.RevokeUserMachineAccessSessionsForUserParams{UserID: userID, Reason: sql.NullString{String: reason, Valid: true}})
	if err != nil {
		return err
	}
	var errs []error
	for _, row := range rows {
		if err := s.revokeCredentialSessions(ctx, machineAccessSession{ID: row.ID, UserID: row.UserID, UserMachineID: row.UserMachineID, EnvironmentID: row.EnvironmentID, CLIClientSessionID: row.CLIClientSessionID, HTTPBaseURL: row.HttpBaseUrl, TerminalSessionID: row.HelperTerminalSessionID, FileSessionID: row.HelperFileSessionID}, reason); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := s.db.Queries().MarkUserMachineAccessSessionRevoked(ctx, row.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ReconcileUserMachineEntitlement is safe to call after every billing
// webhook. It revokes all machines after entitlement loss and the newest
// excess machines after a seat reduction.
func (s *Service) ReconcileUserMachineEntitlement(ctx context.Context, userID string) error {
	if strings.TrimSpace(userID) == "" {
		return errors.New("user-machine entitlement user is required")
	}
	now := s.now().UTC()
	var revokedMachineIDs []string
	active := false
	if err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		seatQuantity, err := tx.Queries().GetActiveUserMachineSeatQuantity(ctx, userID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			active = true
			machines, err := tx.Queries().RevokeUserMachinesOverSeatLimit(ctx, dbsqlc.RevokeUserMachinesOverSeatLimitParams{
				UserID: userID, SeatQuantity: seatQuantity,
			})
			if err != nil {
				return err
			}
			for _, machine := range machines {
				revokedMachineIDs = append(revokedMachineIDs, machine.ID)
				if err := expireAuthenticatedMachineInstallationPairingsTx(ctx, tx, machine.ID, now); err != nil {
					return err
				}
				if err := db.RevokeMachineEnvironmentTx(ctx, tx, machine.EnvironmentID, now); err != nil {
					return err
				}
			}
			return nil
		}
		environments, err := tx.Queries().ListRevokedUserMachineEnvironmentsForUser(ctx, userID)
		if err != nil {
			return err
		}
		for _, environment := range environments {
			if err := expireAuthenticatedMachineInstallationPairingsTx(ctx, tx, environment.ID, now); err != nil {
				return err
			}
			if err := db.RevokeMachineEnvironmentTx(ctx, tx, environment.EnvironmentID, now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if active {
		var errs []error
		for _, userMachineID := range revokedMachineIDs {
			errs = append(errs, s.RevokeUserMachineSessions(ctx, userMachineID, "user_machine_seat_released"))
		}
		return errors.Join(errs...)
	}
	return s.RevokeUserSessions(ctx, userID, "user_machine_entitlement_revoked")
}

// RetryPendingRevocations is intentionally idempotent. Helper's signed
// revocation endpoint accepts repeated session IDs, and marking propagation is
// conditional on a still-pending row.
func (s *Service) RetryPendingRevocations(ctx context.Context) error {
	rows, err := s.db.Queries().ListPendingUserMachineAccessSessionRevocations(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, row := range rows {
		reason := "revoked"
		if row.RevocationReason.Valid && strings.TrimSpace(row.RevocationReason.String) != "" {
			reason = row.RevocationReason.String
		}
		if err := s.revokeCredentialSessions(ctx, machineAccessSession{ID: row.ID, UserID: row.UserID, UserMachineID: row.UserMachineID, EnvironmentID: row.EnvironmentID, CLIClientSessionID: row.CLIClientSessionID, HTTPBaseURL: row.HttpBaseUrl, TerminalSessionID: row.HelperTerminalSessionID, FileSessionID: row.HelperFileSessionID}, reason); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := s.db.Queries().MarkUserMachineAccessSessionRevoked(ctx, row.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type machineAccessSession struct {
	ID, UserID, UserMachineID, EnvironmentID, CLIClientSessionID, HTTPBaseURL string
	TerminalSessionID, FileSessionID                                          string
	CredentialKind                                                            machineCredentialKind
}

func (s *Service) revokeCredentialSessions(ctx context.Context, session machineAccessSession, reason string) error {
	sessionIDs := compactSessionIDs(session.TerminalSessionID, session.FileSessionID)
	if len(sessionIDs) == 0 {
		return nil
	}
	kind := session.CredentialKind
	if kind == "" {
		if session.ID == "" {
			return errors.New("user-machine credential kind is missing")
		}
		if err := s.db.SQL().QueryRowContext(ctx, `SELECT credential_kind FROM user_machine_access_sessions WHERE id=$1`, session.ID).Scan(&kind); err != nil {
			return err
		}
	}
	if kind == machineCredentialSigned {
		// The owning environment/helper revocation fences signed credentials.
		return nil
	}
	if kind != machineCredentialExternal {
		return errors.New("user-machine credential kind is invalid")
	}
	if s.credentials == nil {
		return errors.New("user-machine credential issuer is unavailable")
	}
	if err := s.credentials.RevokeCLI(ctx, access.CredentialRevocationInput{
		UserID: session.UserID, MachineID: session.UserMachineID, EnvironmentID: session.EnvironmentID,
		CLIClientSessionID: session.CLIClientSessionID, HTTPBaseURL: session.HTTPBaseURL,
		SessionIDs: sessionIDs, Reason: reason,
	}); err != nil {
		return fmt.Errorf("revoke user-machine sessions for %s: %w", session.UserMachineID, err)
	}
	return nil
}

func compactSessionIDs(values ...string) []string {
	ids := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !slices.Contains(ids, value) {
			ids = append(ids, value)
		}
	}
	return ids
}

func (s *Service) validatePairing(in PairingInput) error {
	token := strings.TrimSpace(in.EnrollmentToken)
	_, validWorkspace := canonicalWorkspaceRoot(in.Platform, in.WorkspaceRoot)
	publicKey, keyErr := base64.RawURLEncoding.DecodeString(strings.TrimSpace(in.PublicIdentityKey))
	checks := []struct {
		invalid bool
		reason  string
	}{
		{s.policy.PairingLifetime <= 0, "pairing lifetime"},
		{strings.TrimSpace(in.Verifier) == "", "verifier"},
		{invalidMachineAlias(in.Alias), "alias"},
		{invalidMachineDescription(in.Description), "description"},
		{!validMachineArchitecture(in.Architecture), "architecture"},
		{!releases.SupportedPlatformArchitecture(strings.ToLower(strings.TrimSpace(in.Platform)), strings.ToLower(strings.TrimSpace(in.Architecture))), "platform architecture"},
		{!validWorkspace, "workspace root"},
		{!slices.Contains(s.policy.AllowedPlatforms, strings.ToLower(strings.TrimSpace(in.Platform))), "platform"},
		{token != "" && !validEnrollmentToken(token), "enrollment token shape"},
		{keyErr != nil || len(publicKey) != ed25519.PublicKeySize, "public identity key"},
		{(in.SSHUser == "") != (in.SSHPort == 0), "SSH target completeness"},
		{in.SSHUser != "" && !validPairingSSHUser(in.SSHUser), "SSH user"},
	}
	for _, check := range checks {
		if check.invalid {
			return fmt.Errorf("%w: %s", ErrInvalidPairing, check.reason)
		}
	}
	return nil
}

func invalidMachineAlias(value string) bool { return !machinealias.Valid(strings.TrimSpace(value)) }
func invalidMachineDescription(value string) bool {
	value = strings.TrimSpace(value)
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > 160 {
		return true
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func validPairingSSHUser(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n@") {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func canonicalWorkspaceRoot(platform, root string) (string, bool) {
	if strings.ContainsRune(root, '\x00') || strings.TrimSpace(root) != root || root == "" {
		return "", false
	}
	if !strings.EqualFold(strings.TrimSpace(platform), "windows") {
		return root, filepath.IsAbs(root) && filepath.Clean(root) == root
	}
	root = strings.ReplaceAll(root, "/", `\`)
	prefix, remainder := "", root
	switch {
	case len(root) >= 3 && ((root[0] >= 'A' && root[0] <= 'Z') || (root[0] >= 'a' && root[0] <= 'z')) && root[1] == ':' && root[2] == '\\':
		prefix, remainder = strings.ToUpper(root[:1])+root[1:3], root[3:]
	case strings.HasPrefix(root, `\\`):
		parts := strings.Split(root[2:], `\`)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return "", false
		}
		prefix = `\\` + parts[0] + `\` + parts[1]
		remainder = strings.Join(parts[2:], `\`)
	default:
		return "", false
	}
	segments := strings.Split(remainder, `\`)
	clean := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment == "" {
			continue
		}
		if segment == "." || segment == ".." || strings.ContainsAny(segment, `<>:"|?*`) {
			return "", false
		}
		clean = append(clean, segment)
	}
	if len(clean) == 0 {
		return prefix, true
	}
	separator := `\`
	if strings.HasSuffix(prefix, `\`) {
		separator = ""
	}
	canonical := prefix + separator + strings.Join(clean, `\`)
	return canonical, canonical == root
}

func validMachineArchitecture(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "amd64" || value == "arm64"
}

func mapMachine(row dbsqlc.UserMachine) UserMachine {
	diagnostics := RuntimeDiagnostics{WorkerGeneration: uint64(row.WorkerGeneration), OSBootID: row.OsBootID.String, WorkerServiceScope: row.WorkerServiceScope, ConnectorState: row.ConnectorState, ConnectorGeneration: uint64(row.ConnectorGeneration)}
	if row.RuntimeDiagnosticsObservedAt.Valid {
		observed := row.RuntimeDiagnosticsObservedAt.Time
		diagnostics.ObservedAt = &observed
	}
	m := UserMachine{Ownership: "personal", OwnerAccount: row.UserID, Permissions: []string{}, ID: row.ID, EnvironmentID: row.EnvironmentID, Description: row.Description, Alias: row.Alias, Platform: row.Platform, Architecture: row.Architecture, WorkspaceRoot: row.WorkspaceRoot, State: row.State, SeatState: row.SeatState, Online: row.Online, RuntimeVersions: row.RuntimeVersions, Capabilities: mapCapabilities(row.ConfiguredCapabilities, row.ObservedCapabilities), MachineCapabilities: mapMachineCapabilityPolicy(row), PublicIdentityKey: row.PublicIdentityKey.String, InstallationGeneration: row.InstallationGeneration, Availability: mapAvailability(row), RuntimeDiagnostics: diagnostics}
	if row.OwnerTeamID.Valid {
		m.Ownership = "team"
		m.OwnerTeamID = row.OwnerTeamID.String
		m.OwnerAccount = ""
		m.Shared = true
	}
	if row.EnrolledAt.Valid {
		v := row.EnrolledAt.Time
		m.EnrolledAt = &v
	}
	if row.LastSeenAt.Valid {
		v := row.LastSeenAt.Time
		m.LastSeenAt = &v
	}
	return m
}

func allocateMachineAlias(ctx context.Context, queries *dbsqlc.Queries, userID, alias string) (string, error) {
	if err := queries.LockUserMachineAliases(ctx, userID); err != nil {
		return "", err
	}
	alias = strings.TrimSpace(alias)
	exists, err := queries.UserMachineAliasExists(ctx, dbsqlc.UserMachineAliasExistsParams{UserID: userID, Alias: alias})
	if err != nil {
		return "", err
	}
	if exists {
		return "", ErrMachineNameConflict
	}
	return alias, nil
}

// A fresh identity must never take over an existing machine merely because its
// host has the same name. Keep the old machine and choose the next free name.
func allocateFreshMachineAlias(ctx context.Context, queries *dbsqlc.Queries, userID, alias string) (string, error) {
	if err := queries.LockUserMachineAliases(ctx, userID); err != nil {
		return "", err
	}
	for ordinal := 1; ordinal <= 1000; ordinal++ {
		candidate := machinealias.Candidate(alias, ordinal)
		exists, err := queries.UserMachineAliasExists(ctx, dbsqlc.UserMachineAliasExistsParams{UserID: userID, Alias: candidate})
		if err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
	}
	return "", ErrMachineNameConflict
}

func mapCapabilities(configured, observed []string) MachineCapabilities {
	capability := func(name string) CapabilityAvailability {
		return CapabilityAvailability{Configured: slices.Contains(configured, name), Observed: slices.Contains(observed, name)}
	}
	return MachineCapabilities{FileReceive: capability("file_receive"), PreviewLaunch: capability("preview_launch"), TerminalHost: capability("terminal_host"), SessionHost: capability("session_host"), KeepAwake: capability("keep_awake"), EnvironmentInjection: capability("environment_injection")}
}
func newID(prefix string) string { return prefix + "_" + uuid.NewString() }
func randomCode(length int) (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, length)
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(raw[i])%len(alphabet)]
	}
	return string(b), nil
}

const enrollmentTokenLength = 26

func validEnrollmentToken(token string) bool {
	_, ok := enrollmentTokenSecret(strings.TrimSpace(token))
	return ok
}

func enrollmentTokenSecret(token string) (string, bool) {
	token = strings.TrimSpace(token)
	if len(token) != enrollmentTokenLength {
		return "", false
	}
	for i := range token {
		if !((token[i] >= '0' && token[i] <= '9') || (token[i] >= 'A' && token[i] <= 'Z')) {
			return "", false
		}
	}
	return token[1:], true
}

func enrollmentTokenHash(token string) [sha256.Size]byte {
	token = strings.TrimSpace(token)
	return sha256.Sum256([]byte(token))
}

// randomEnrollmentToken returns one URL-safe credential. The first character
// carries installer-platform parity and the remaining characters carry the
// secret entropy.
func randomEnrollmentToken() (string, error) {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	metadata, err := randomCodeFromAlphabet(alphabet, 1)
	if err != nil {
		return "", err
	}
	rest, err := randomCodeFromAlphabet(alphabet, enrollmentTokenLength-1)
	if err != nil {
		return "", err
	}
	return metadata + rest, nil
}

func randomEnrollmentTokenFor(shell string) (string, error) {
	shell = strings.ToLower(strings.TrimSpace(shell))
	if shell != "posix" && shell != "powershell" {
		return "", errors.New("enrollment shell is invalid")
	}
	chars := "13579ACEGIKMOQSUWY"
	if shell == "posix" {
		chars = "02468BDFHJLNPRTVXZ"
	}
	metadata, err := randomCodeFromAlphabet(chars, 1)
	if err != nil {
		return "", err
	}
	rest, err := randomCodeFromAlphabet("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ", enrollmentTokenLength-1)
	if err != nil {
		return "", err
	}
	return metadata + rest, nil
}

func randomCodeFromAlphabet(alphabet string, length int) (string, error) {
	b := make([]byte, length)
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(raw[i])%len(alphabet)]
	}
	return string(b), nil
}

func (s *Service) cliTokenPolicy() auth.ClientTokenPolicy {
	return auth.ClientTokenPolicy{HashKey: s.cliHashKey, AccessLifetime: s.cliAccessLifetime, RefreshLifetime: s.cliRefreshLifetime}
}
