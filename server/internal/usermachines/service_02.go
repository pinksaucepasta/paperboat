package usermachines

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat/server/internal/accessdescriptor"
	"github.com/pinksaucepasta/paperboat/server/internal/admission"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
	"github.com/pinksaucepasta/paperboat/server/internal/releases"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"slices"
	"strconv"
	"strings"
	"time"
)

func (s *Service) validateAuthenticatedMachineInstallationMachine(machine UserMachine, in AuthenticatedMachineInstallationInput) (UserMachine, error) {
	if machine.SeatState != "occupied" ||
		machine.InstallationGeneration != in.InstallationGeneration || machine.PublicIdentityKey != in.PublicIdentityKey {
		return UserMachine{}, ErrInvalidMachineInstallation
	}
	pairingInput := PairingInput{
		Verifier: in.Verifier, Alias: machine.Alias, Platform: machine.Platform,
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
		pairing.RequestedAlias.String == machine.Alias && pairing.Platform.String == machine.Platform && pairing.Architecture.String == machine.Architecture &&
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
		binding.Generation != in.InstallationGeneration ||
		binding.ReuseIdentity != in.CanReuseRuntimeIdentity ||
		!s.authenticatedMachineInstallationMaterialMatches(pairing.InstallationConfigCiphertext, machine.ID, machine.EnvironmentID, in.InstallationGeneration, pairing.AuthenticatedSetupHelperEnrollmentID) {
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
	Alias, Platform, Architecture, WorkspaceRoot, PublicIdentityKey string
	RuntimeVersions                                                 json.RawMessage
	AcceptBetaPlatform                                              bool
	KeepAwake                                                       bool
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
	if userID == "" || invalidMachineAlias(in.Alias) || !validMachineArchitecture(in.Architecture) ||
		!releases.SupportedPlatformArchitecture(strings.ToLower(strings.TrimSpace(in.Platform)), strings.ToLower(strings.TrimSpace(in.Architecture))) ||
		!validWorkspace ||
		!slices.Contains(s.policy.AllowedPlatforms, strings.ToLower(strings.TrimSpace(in.Platform))) ||
		keyErr != nil || len(publicKey) != ed25519.PublicKeySize {
		return UserMachine{}, ErrInvalidSetup
	}
	if s.issuer == "" || s.helperListenPort == 0 {
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
			row, err = tx.Queries().UpdateUserMachineMetadata(ctx, dbsqlc.UpdateUserMachineMetadataParams{ID: row.ID, UserID: userID, Alias: strings.TrimSpace(in.Alias), RuntimeVersions: in.RuntimeVersions})
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
			ID: machineID, UserID: userID, EnvironmentID: machineID, Alias: alias,
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
		result.Installation = &ClientInstallation{ControlURL: s.issuer, HelperListenAddress: fmt.Sprintf("127.0.0.1:%d", s.helperListenPort)}
	}
	return result, err
}

func (s *Service) completeAuthenticatedMachineInstallationTx(ctx context.Context, tx *db.Tx, userID string, row dbsqlc.UserMachine, in SetupInput) (dbsqlc.UserMachine, error) {
	var err error
	row, err = tx.Queries().UpdateUserMachineInstallation(ctx, dbsqlc.UpdateUserMachineInstallationParams{
		ID: row.ID, UserID: userID, Alias: strings.TrimSpace(in.Alias),
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
		if err := ensureMachineAddressLease(ctx, tx, row.ID); err != nil {
			return dbsqlc.UserMachine{}, err
		}
		row.SeatState = "occupied"
	} else if row.SeatState != "occupied" {
		return dbsqlc.UserMachine{}, ErrEnrollmentState
	}
	if err := s.ensureHelperRoute(ctx, tx, row.ID, row.EnvironmentID); err != nil {
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
	params := dbsqlc.CreateUserMachinePairingParams{VerifierHash: verifierHash[:], UserCode: sql.NullString{String: code, Valid: true}, RequestedAlias: sql.NullString{String: strings.TrimSpace(in.Alias), Valid: true}, Platform: sql.NullString{String: strings.ToLower(strings.TrimSpace(in.Platform)), Valid: true}, Architecture: sql.NullString{String: strings.ToLower(strings.TrimSpace(in.Architecture)), Valid: true}, WorkspaceRoot: sql.NullString{String: in.WorkspaceRoot, Valid: true}, RuntimeVersions: in.RuntimeVersions, PublicIdentityKey: sql.NullString{String: strings.TrimSpace(in.PublicIdentityKey), Valid: true}, CanReuseRuntimeIdentity: in.CanReuseRuntimeIdentity, KeepAwake: in.KeepAwake, ExpiresAt: expires, ReplacesMachineID: sql.NullString{String: strings.TrimSpace(in.ReplacesMachineID), Valid: strings.TrimSpace(in.ReplacesMachineID) != ""}}
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
				return ErrEnrollmentTokenInvalid
			}
			if err != nil {
				return err
			}
			switch enrollment.InstallationState {
			case "cancelled", "revoked", "deleted":
				return ErrEnrollmentTokenInvalid
			}
			if err := authorizeResetPairingTx(ctx, tx, enrollment.ID, enrollment.Generation, verifierHash[:]); err != nil {
				return err
			}
			// A claimed verifier identifies a protected installation journal even
			// after expiry; never tell its owner this is a fresh expired grant.
			if len(enrollment.VerifierHash) != 0 {
				return ErrEnrollmentTokenUsed
			}
			if enrollment.InstallationState == "expired" || !s.now().UTC().Before(enrollment.ExpiresAt) {
				return ErrEnrollmentTokenExpired
			}
			if enrollment.InstallationState != "awaiting_bootstrap" {
				return ErrEnrollmentTokenUsed
			}
			enrollmentUserID = enrollment.BootstrapOwnerAccount.String
			enrollmentID = enrollment.ID
			if enrollment.ExpiresAt.Before(params.ExpiresAt) {
				params.ExpiresAt = enrollment.ExpiresAt
			}
			row, err = tx.Queries().ClaimBootstrapPairing(ctx, dbsqlc.ClaimBootstrapPairingParams{
				ID: enrollment.ID, VerifierHash: params.VerifierHash, UserCode: params.UserCode,
				RequestedAlias: params.RequestedAlias,
				Platform:       params.Platform, Architecture: params.Architecture, WorkspaceRoot: params.WorkspaceRoot,
				RuntimeVersions: params.RuntimeVersions, PublicIdentityKey: params.PublicIdentityKey,
				SshUser: params.SshUser, SshPort: params.SshPort,
				CanReuseRuntimeIdentity: params.CanReuseRuntimeIdentity, KeepAwake: params.KeepAwake, ReplacesMachineID: params.ReplacesMachineID,
			})
			if errors.Is(err, sql.ErrNoRows) {
				return ErrEnrollmentTokenExpired
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
				// The console names a refusal it can act on (remove machines or upgrade).
				FailureCode: enrollmentFailureCode(err),
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

func enrollmentFailureCode(err error) sql.NullString {
	var limit *planlimits.LimitError
	if errors.As(err, &limit) {
		return sql.NullString{String: limit.Code(), Valid: true}
	}
	return sql.NullString{}
}

type UserMachine struct {
	Ownership    string   `json:"ownership"`
	OwnerAccount string   `json:"owner_account,omitempty"`
	OwnerTeamID  string   `json:"owner_team_id,omitempty"`
	Permissions  []string `json:"permissions"`
	// Tags are the machine's tags in the selected workspace's policy file (tag:<name>).
	Tags                   []string                `json:"tags"`
	CanManage              bool                    `json:"can_manage"`
	Shared                 bool                    `json:"shared"`
	ID                     string                  `json:"id"`
	EnvironmentID          string                  `json:"environment_id"`
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
	SSH                    *MachineSSH             `json:"ssh,omitempty"`
}

type ClientInstallation struct {
	ControlURL          string `json:"control_url"`
	HelperListenAddress string `json:"helper_listen_address"`
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
	out := Overview{BootstrapCommand: s.bootstrapCommand}
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		usage, err := planlimits.UsageTx(ctx, tx, planlimits.Scope{Account: userID})
		if err != nil {
			return err
		}
		machines := usage[planlimits.Machines]
		out.MachineCount = int32(machines.Used)
		if machines.Limit != nil {
			limit := int32(*machines.Limit)
			available := max(int32(0), limit-out.MachineCount)
			out.MachineLimit, out.AvailableMachines = &limit, &available
		}
		return nil
	})
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
		row, err := s.machineForCapability(ctx, tx.Queries(), dbsqlc.GetUserMachineForCapabilityParams{
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
		row, err := s.machineForCapability(ctx, tx.Queries(), dbsqlc.GetUserMachineForCapabilityParams{
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

// ExecDescriptor issues an exec operation credential. nativeOnly is the client's statement that it can run
// the operation over its native peer connection alone: when the machine has no current edge route (its edge
// carrier is re-attaching after an outage), such a client still gets the descriptor, with no edge endpoints,
// instead of machine_not_ready. Released clients do not send it and keep the old answer.
// Prior art: Tailscale SSH authorizes per connection from the netmap, independent of DERP reachability
// (ssh/tailssh/tailssh.go).
func (s *Service) ExecDescriptor(ctx context.Context, userID, sourceMachineID, userMachineID, cliClientSessionID, operationID string, nativeOnly bool) (ExecDescriptor, error) {
	return s.operationDescriptor(ctx, userID, sourceMachineID, userMachineID, cliClientSessionID, operationID, "exec_operation", "exec:operate", ErrExecOperationInvalid, nativeOnly)
}

func (s *Service) SSHDescriptor(ctx context.Context, userID, sourceMachineID, userMachineID, cliClientSessionID, operationID string, nativeOnly bool) (SSHDescriptor, error) {
	return s.operationDescriptor(ctx, userID, sourceMachineID, userMachineID, cliClientSessionID, operationID, "ssh_operation", "ssh:operate", ErrSSHOperationInvalid, nativeOnly)
}

func (s *Service) operationDescriptor(ctx context.Context, userID, sourceMachineID, userMachineID, cliClientSessionID, operationID, credentialClass, scope string, invalidOperationErr error, nativeOnly bool) (ExecDescriptor, error) {
	if !execOperationIDPattern.MatchString(operationID) {
		return ExecDescriptor{}, invalidOperationErr
	}
	if sourceMachineID == "" || cliClientSessionID == "" {
		return ExecDescriptor{}, ErrNotFound
	}
	if allowed, err := s.sourceMachineAllowed(ctx, userID, sourceMachineID); err != nil {
		return ExecDescriptor{}, err
	} else if !allowed {
		return ExecDescriptor{}, ErrNotFound
	}
	capability := "exec"
	if credentialClass == "ssh_operation" {
		capability = "managed_ssh"
	}
	row, err := s.machineForCapability(ctx, s.db.Queries(), dbsqlc.GetUserMachineForCapabilityParams{ID: userMachineID, UserID: userID, Capability: capability})
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
		target, targetErr := s.db.Queries().GetMachineSSHTarget(ctx, row.ID)
		if targetErr != nil || target.MachineGeneration != row.InstallationGeneration {
			if targetErr != nil && !errors.Is(targetErr, sql.ErrNoRows) {
				return ExecDescriptor{}, targetErr
			}
			return ExecDescriptor{}, ErrMachineCapabilityUnavailable
		}
		keys, keysErr := s.db.Queries().GetActiveMachineSSHHostKeySet(ctx, row.ID)
		if keysErr != nil || keys.MachineGeneration != row.InstallationGeneration {
			if keysErr != nil && !errors.Is(keysErr, sql.ErrNoRows) {
				return ExecDescriptor{}, keysErr
			}
			return ExecDescriptor{}, ErrMachineCapabilityUnavailable
		}
	}
	route, err := s.db.Queries().GetActiveHelperRouteForEnvironment(ctx, row.EnvironmentID)
	edge := err == nil
	if errors.Is(err, sql.ErrNoRows) && !nativeOnly {
		return ExecDescriptor{}, ErrMachineCapabilityUnavailable
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ExecDescriptor{}, err
	}
	if s.controlSigner == nil {
		return ExecDescriptor{}, errors.New("user-machine exec credential issuer is unavailable")
	}
	issuedAt := s.now().UTC()
	expiresAt := admission.Expiry(s.controlSigner, issuedAt)
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
	// Without an edge route the session has no edge base URL: these signed credentials are fenced by the
	// environment, never revoked through the edge, and the client runs the operation natively.
	httpIngressURL, endpoints := "", map[string]any{}
	if edge {
		var ingressErr error
		if httpIngressURL, ingressErr = s.runtimeIngressURL(ctx, row.EnvironmentID, route.PublicHost); ingressErr != nil {
			return ExecDescriptor{}, ingressErr
		}
		endpoints = machineTerminalEndpoints(runtimeWebsocketBase(httpIngressURL))
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
		Endpoints:   endpoints,
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
