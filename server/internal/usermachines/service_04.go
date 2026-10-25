package usermachines

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
	"strings"
	"time"
)

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
				if n, err := occupySeat(ctx, tx, row.ID, userID); err != nil || n != 1 {
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
					if n, err := occupySeat(ctx, tx, existing.ID, userID); err != nil || n != 1 {
						if err != nil {
							return err
						}
						return ErrEnrollmentState
					}
				} else if existing.SeatState != "occupied" {
					return ErrEnrollmentState
				}
				row, err := tx.Queries().UpdateUserMachineInstallation(ctx, dbsqlc.UpdateUserMachineInstallationParams{ID: existing.ID, UserID: userID, Alias: existing.Alias, WorkspaceRoot: pairing.WorkspaceRoot.String, RuntimeVersions: pairing.RuntimeVersions})
				if err != nil {
					return err
				}

				if err := s.ensureHelperRoute(ctx, tx, row.ID, row.EnvironmentID); err != nil {
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
				if err := placeEnrolledMachine(ctx, tx, userID, pairing, row.ID); err != nil {
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
			retiredID, err := s.retireForFreshEnrollmentTx(ctx, tx, userID, pairing.ReplacesMachineID.String, machineID)
			if err != nil {
				return err
			}
			alias, err := allocateFreshMachineAlias(ctx, tx.Queries(), userID, pairing.RequestedAlias.String)
			if err != nil {
				return err
			}
			row, err := tx.Queries().CreateEnrolledMachine(ctx, dbsqlc.CreateEnrolledMachineParams{
				ID: machineID, UserID: userID, EnvironmentID: machineID, Alias: alias,
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
			if n, err := occupySeat(ctx, tx, row.ID, userID); err != nil || n != 1 {
				if err != nil {
					return err
				}
				return ErrEnrollmentState
			}
			row, err = tx.Queries().UpdateUserMachineInstallation(ctx, dbsqlc.UpdateUserMachineInstallationParams{ID: row.ID, UserID: userID, Alias: alias, WorkspaceRoot: pairing.WorkspaceRoot.String, RuntimeVersions: pairing.RuntimeVersions})
			if err != nil {
				return err
			}
			if retiredID != "" {
				if err := s.finishFreshReplacementTx(ctx, tx, userID, retiredID, row); err != nil {
					return err
				}
			}

			if err := s.ensureHelperRoute(ctx, tx, row.ID, row.EnvironmentID); err != nil {
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
			if err := placeEnrolledMachine(ctx, tx, userID, pairing, row.ID); err != nil {
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
				if n, err := occupySeat(ctx, tx, existing.ID, userID); err != nil || n != 1 {
					if err != nil {
						return err
					}
					return ErrEnrollmentState
				}
			} else if existing.SeatState != "occupied" {
				return ErrEnrollmentState
			}
			row, err := tx.Queries().UpdateUserMachineInstallation(ctx, dbsqlc.UpdateUserMachineInstallationParams{
				ID: existing.ID, UserID: userID, Alias: pairing.RequestedAlias.String,
				WorkspaceRoot: pairing.WorkspaceRoot.String, RuntimeVersions: pairing.RuntimeVersions,
			})
			if err != nil {
				return err
			}

			if err := s.ensureHelperRoute(ctx, tx, row.ID, row.EnvironmentID); err != nil {
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

func (s *Service) buildInstallationMaterial(ctx context.Context, userID string, pairing dbsqlc.UserMachinePairing, machine UserMachine, operationKey string, existingClientSession json.RawMessage, issueCLICredentials, reuseRuntimeIdentity bool, expectedHelperID string, authorityGuard HelperEnrollmentAuthorityGuard) (installationMaterialBuild, error) {
	if strings.TrimSpace(s.encryptionKey) == "" {
		return installationMaterialBuild{}, errors.New("user-machine provisioning encryption is not configured")
	}
	if s.helperGrant == nil {
		return installationMaterialBuild{}, ErrProvisioningUnavailable
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
	if len(existingClientSession) == 0 && issueCLICredentials && s.cliClientID != "" && s.cliScopes != "" && len(s.cliHashKey) > 0 && s.cliAccessLifetime > 0 && s.cliRefreshLifetime > 0 {
		cliSessionID = newID("session")
		cliAccessToken, cliRefreshToken = oneShotToken(32), oneShotToken(32)
	}
	materialMap := map[string]any{
		"schema": "paperboat.machine-installation/v1", "user_machine_id": machine.ID, "pairing_id": installationEnrollmentID, "environment_id": machine.EnvironmentID,
		"control_url": s.issuer, "helper_id": grant.HelperID, "enrollment_id": grant.EnrollmentID,
		"enrollment_credential": grant.Credential, "reuse_identity": reuseIdentity, "expires_at": grant.ExpiresAt,
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
	build, err := s.buildInstallationMaterial(ctx, userID, pairing, machine, operationKey, nil, true, pairing.CanReuseRuntimeIdentity, "", nil)
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
	var authenticatedBinding *authenticatedMachineInstallationMaterialBinding
	var materialSessionID string
	if len(pairing.InstallationConfigCiphertext) > 0 {
		plaintext, err := secrets.Decrypt(s.encryptionKey, pairing.InstallationConfigCiphertext)
		if err != nil {
			return nil, ErrInstallationUnavailable
		}
		if runtimeEnrolled {
			binding, bindErr := authenticatedMachineInstallationMaterialBindingFromPlaintext([]byte(plaintext))
			if bindErr != nil || binding.MachineID != machine.ID || binding.EnvironmentID != machine.EnvironmentID ||
				binding.Generation != machine.InstallationGeneration || machine.PublicIdentityKey != pairing.PublicIdentityKey.String ||
				machine.SeatState != "occupied" || machine.State == "revoked" || machine.State == "deleted" {
				return nil, ErrInstallationUnavailable
			}
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
		}
	}
	var authorityGuard HelperEnrollmentAuthorityGuard
	if authenticatedBinding != nil {
		authorityGuard = s.authenticatedMachineInstallationRecoveryAuthorityGuard(userID, pairing, *authenticatedBinding, materialSessionID)
	}
	build, err := s.buildInstallationMaterial(ctx, userID, pairing, machine, operationKey, existingClientSession, !runtimeEnrolled, runtimeEnrolled, expectedHelperID, authorityGuard)
	if err != nil {
		return nil, err
	}
	if len(existingClientSession) > 0 && !runtimeEnrolled {
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
			if !s.authenticatedMachineInstallationMaterialMatches(build.ciphertext, machine.ID, machine.EnvironmentID, machine.InstallationGeneration, current.AuthenticatedSetupHelperEnrollmentID) {
				return ErrInstallationUnavailable
			}
		}
		if materialSessionID != "" {
			identity, identityErr := tx.Queries().GetClientSessionForInstallationRecovery(ctx, materialSessionID)
			if identityErr != nil || identity.UserID != userID || identity.ClientID != s.cliClientID {
				return ErrInstallationUnavailable
			}
			if bound, bindErr := tx.Queries().BindCLIClientSessionToUserMachine(ctx, dbsqlc.BindCLIClientSessionToUserMachineParams{
				TargetMachineID: sql.NullString{String: machine.ID, Valid: true}, TargetCLISessionID: materialSessionID, TargetUserID: userID,
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
	MachineID     string `json:"user_machine_id"`
	EnvironmentID string `json:"environment_id"`
	HelperID      string `json:"helper_id"`
	EnrollmentID  string `json:"enrollment_id"`
	ReuseIdentity bool   `json:"reuse_identity"`
	Generation    int64  `json:"installation_generation"`
}

func authenticatedMachineInstallationMaterialBindingFromPlaintext(material []byte) (authenticatedMachineInstallationMaterialBinding, error) {
	var binding authenticatedMachineInstallationMaterialBinding
	if err := json.Unmarshal(material, &binding); err != nil || binding.MachineID == "" || binding.EnvironmentID == "" || binding.HelperID == "" ||
		(!binding.ReuseIdentity && binding.EnrollmentID == "") || binding.Generation < 1 {
		return authenticatedMachineInstallationMaterialBinding{}, ErrInstallationUnavailable
	}
	return binding, nil
}

func (s *Service) authenticatedMachineInstallationMaterialMatches(ciphertext []byte, machineID, environmentID string, generation int64, helperEnrollmentID sql.NullString) bool {
	plaintext, err := secrets.Decrypt(s.encryptionKey, ciphertext)
	if err != nil {
		return false
	}
	binding, err := authenticatedMachineInstallationMaterialBindingFromPlaintext([]byte(plaintext))
	if err != nil || binding.MachineID != machineID || binding.EnvironmentID != environmentID || binding.Generation != generation {
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
		if machine.EnvironmentID != binding.EnvironmentID || machine.SeatState != "occupied" ||
			machine.InstallationGeneration != binding.Generation || machine.PublicIdentityKey != expected.PublicIdentityKey.String {
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

		current.RequestedAlias.String == machine.Alias && current.Platform.String == machine.Platform && current.Architecture.String == machine.Architecture &&
		current.WorkspaceRoot.String == machine.WorkspaceRoot && bytes.Equal(current.RuntimeVersions, machine.RuntimeVersions) &&
		current.SshUser == expected.SshUser && current.SshPort == expected.SshPort && current.CanReuseRuntimeIdentity == expected.CanReuseRuntimeIdentity &&
		current.State == "consumed" && current.InstallationConfigConsumedAt.Valid && current.InstallationConfigConsumedAt == expected.InstallationConfigConsumedAt &&
		current.InstallationRecoveryOperationKey.Valid && current.InstallationRecoveryOperationKey == expected.InstallationRecoveryOperationKey &&
		bytes.Equal(current.InstallationConfigCiphertext, expected.InstallationConfigCiphertext) && current.ExpiresAt.Equal(expected.ExpiresAt) && now.Before(current.ExpiresAt) &&
		binding.MachineID == machine.ID && binding.EnvironmentID == machine.EnvironmentID
}

func replaceInstallationClientSession(material []byte, session installationClientSession) ([]byte, error) {
	var fields map[string]any
	if err := json.Unmarshal(material, &fields); err != nil {
		return nil, err
	}
	fields["client_session"] = session
	return json.Marshal(fields)
}

// ensureMachineAddressLease gives a machine its local-address lease when its seat is occupied, in the same
// transaction. Since 05862b9b machine lists only read leases (allocating there took a table lock per list), so
// pairing is where a new machine gets one; without it the daemon cannot bind the machine's local address and
// private access fails (prod 2026-10-25: 14 machines had none).
func ensureMachineAddressLease(ctx context.Context, tx *db.Tx, machineID string) error {
	_, err := tx.Exec(ctx, `SELECT paperboat.ensure_control_sync_lease($1)`, machineID)
	return err
}

// occupySeat takes the machine's seat at pairing together with its local-address lease: every approve path
// occupies the seat here, so none can leave a paired machine without an address.
func occupySeat(ctx context.Context, tx *db.Tx, machineID, userID string) (int64, error) {
	n, err := tx.Queries().OccupyUserMachineSeat(ctx, dbsqlc.OccupyUserMachineSeatParams{ID: machineID, UserID: userID})
	if err != nil || n != 1 {
		return n, err
	}
	return n, ensureMachineAddressLease(ctx, tx, machineID)
}
