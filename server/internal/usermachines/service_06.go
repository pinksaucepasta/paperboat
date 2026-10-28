package usermachines

import (
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
	"github.com/pinksaucepasta/paperboat/server/internal/access"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/environment"
	"github.com/pinksaucepasta/paperboat/server/internal/machinealias"
	"github.com/pinksaucepasta/paperboat/server/internal/releases"
	"path/filepath"
	"slices"
	"strings"
)

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
		if err := environment.LockVaultMutationsTx(ctx, tx); err != nil {
			return err
		}
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
				if err := db.FenceRemovedMachineVaultTx(ctx, tx, userID, machine.ID); err != nil {
					return err
				}
				if err := db.CleanupMachineMachineTx(ctx, tx, userID, machine.ID, now); err != nil {
					return err
				}

				if err := expireAuthenticatedMachineInstallationPairingsTx(ctx, tx, machine.ID, now); err != nil {
					return err
				}
				if err := db.RevokeMachineEnvironmentTx(ctx, tx, machine.EnvironmentID, now); err != nil {
					return err
				}
			}
			return nil
		}
		revoked, err := tx.Queries().RevokeUserMachinesForEntitlement(ctx, userID)
		if err != nil {
			return err
		}
		for _, id := range revoked {
			if err := db.FenceRemovedMachineVaultTx(ctx, tx, userID, id); err != nil {
				return err
			}
			if err := db.CleanupMachineMachineTx(ctx, tx, userID, id, now); err != nil {
				return err
			}
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
	if err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		machines, err := tx.Queries().FinalDeleteUserMachines(ctx)
		if err != nil {
			return fmt.Errorf("final delete removed machines: %w", err)
		}
		for _, machine := range machines {
			if err := s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: machine.UserID, ActorType: audit.ActorSystem,
				EventType: "user_machine.final_deleted", ResourceType: "user_machine", ResourceID: machine.ID,
				IdempotencyKey: "user_machine.final_deleted:" + machine.ID, Metadata: map[string]any{"alias": machine.Alias}}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
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
	if err := s.validatePairingPlatform(in.EnrollmentToken, in.Platform, in.Architecture); err != nil {
		return err
	}
	_, validWorkspace := canonicalWorkspaceRoot(in.Platform, in.WorkspaceRoot)
	publicKey, keyErr := base64.RawURLEncoding.DecodeString(strings.TrimSpace(in.PublicIdentityKey))
	checks := []struct {
		invalid bool
		reason  string
	}{
		{s.policy.PairingLifetime <= 0, "pairing lifetime"},
		{strings.TrimSpace(in.Verifier) == "", "verifier"},
		{invalidMachineAlias(in.Alias), "alias"},
		{!validWorkspace, "workspace root"},
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

func (s *Service) validatePairingPlatform(token, platform, architecture string) error {
	token = strings.TrimSpace(token)
	if token != "" && !validEnrollmentToken(token) {
		return ErrEnrollmentTokenInvalid
	}
	platform, architecture = strings.ToLower(strings.TrimSpace(platform)), strings.ToLower(strings.TrimSpace(architecture))
	if !releases.SupportedPlatformArchitecture(platform, architecture) || !slices.Contains(s.policy.AllowedPlatforms, platform) ||
		token != "" && EnrollmentTokenUsesPowerShell(token) != (platform == "windows") {
		return ErrPairingPlatformUnsupported
	}
	return nil
}

func invalidMachineAlias(value string) bool { return !machinealias.Valid(strings.TrimSpace(value)) }

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

// reportedRuntimeVersions is the version the machine's runtime reported in its latest observation. The
// runtime_versions column is written once, at pairing, and every update after it would leave it behind.
func reportedRuntimeVersions(row dbsqlc.UserMachine) json.RawMessage {
	if !row.ReporterVersion.Valid || row.ReporterVersion.String == "" {
		return row.RuntimeVersions
	}
	encoded, _ := json.Marshal(map[string]string{"pb": row.ReporterVersion.String}) // a string map always encodes
	return encoded
}

func mapMachine(row dbsqlc.UserMachine) UserMachine {
	diagnostics := RuntimeDiagnostics{WorkerGeneration: uint64(row.WorkerGeneration), OSBootID: row.OsBootID.String, WorkerServiceScope: row.WorkerServiceScope, ConnectorState: row.ConnectorState, ConnectorGeneration: uint64(row.ConnectorGeneration)}
	// Unchanged diagnostics are not rewritten; the heartbeat that confirms them refreshes last_seen_at.
	if row.RuntimeDiagnosticsObservedAt.Valid {
		observed := row.RuntimeDiagnosticsObservedAt.Time
		if row.LastSeenAt.Valid && row.LastSeenAt.Time.After(observed) {
			observed = row.LastSeenAt.Time
		}
		diagnostics.ObservedAt = &observed
	}
	m := UserMachine{Ownership: "personal", OwnerAccount: row.UserID, Permissions: []string{}, ID: row.ID, EnvironmentID: row.EnvironmentID, Alias: row.Alias, Platform: row.Platform, Architecture: row.Architecture, WorkspaceRoot: row.WorkspaceRoot, State: row.State, SeatState: row.SeatState, Online: row.Online, RuntimeVersions: reportedRuntimeVersions(row), InstalledRuntimeVersions: row.RuntimeVersions, Capabilities: mapCapabilities(row.ConfiguredCapabilities, row.ObservedCapabilities), MachineCapabilities: mapMachineCapabilityPolicy(row), PublicIdentityKey: row.PublicIdentityKey.String, InstallationGeneration: row.InstallationGeneration, Availability: mapAvailability(row), RuntimeDiagnostics: diagnostics}
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
// host has the same name. Keep the old machine and choose the next free name; a
// retired record of the same computer is removed later, when its SSH host keys
// prove it (ReplaceRetiredMachineTx), and the fresh machine then takes the name.
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
const posixEnrollmentMetadata = "02468BDFHJLNPRTVXZ"
const powershellEnrollmentMetadata = "13579ACEGIKMOQSUWY"

// EnrollmentTokenUsesPowerShell is the installer and pairing shell contract.
func EnrollmentTokenUsesPowerShell(token string) bool {
	token = strings.TrimSpace(token)
	return validEnrollmentToken(token) && strings.ContainsRune(powershellEnrollmentMetadata, rune(token[0]))
}

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
	chars := powershellEnrollmentMetadata
	if shell == "posix" {
		chars = posixEnrollmentMetadata
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
