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
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/machinealias"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
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
	sessions := make([]dbsqlc.RevokeUserMachineAccessSessionsRow, len(rows))
	for i, row := range rows {
		sessions[i] = dbsqlc.RevokeUserMachineAccessSessionsRow(row)
	}
	var errs []error
	if err := s.revokeSessionCredentials(ctx, sessions); err != nil {
		errs = append(errs, err)
	}
	// One machine per transaction, like headscale's per-node DeleteNode (hscontrol/db/node.go): a serialization
	// failure dooms the whole transaction, so a shared batch transaction failed every later machine's statements
	// (2,000 errors for 34 removals on 11-16) and replayed the batch. A machine that still fails is skipped for this
	// tick, so one bad row never stalls the rest. Non-nil: pgx sends nil as NULL, and NOT (id = ANY(NULL)) is NULL.
	skipped := []string{}
	for range finalDeleteBatch {
		var id string
		found, more := false, false
		err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			machines, err := tx.Queries().LockUserMachinesForFinalDeletion(ctx, skipped)
			if err != nil || len(machines) == 0 {
				found = false
				return err
			}
			found, id = true, machines[0].ID
			more, err = finalDeleteMachine(ctx, tx, s.audit, machines[0])
			return err
		})
		if more && err == nil {
			if s.afterFinalDeleteBatch != nil {
				s.afterFinalDeleteBatch()
			}
			continue // a batch of child rows went; the machine stays a candidate for the next iteration
		}
		if !found {
			if err != nil {
				errs = append(errs, fmt.Errorf("final delete removed machines: %w", err))
			}
			break
		}
		if err != nil {
			skipped = append(skipped, id)
			slog.WarnContext(ctx, "final delete skipped removed machine", "machine_id", id, "error", err)
		}
	}
	if len(skipped) > 0 {
		errs = append(errs, fmt.Errorf("final delete skipped %d removed machines; see the machine_id warnings", len(skipped)))
	}
	return errors.Join(errs...)
}

// finalDeleteBatch bounds the machines one RetryPendingRevocations tick deletes.
const finalDeleteBatch = 100

// finalDeleteChildren are the tables whose rows a machine can gather by the thousand (one access session per exec or
// terminal); each final-delete transaction removes at most finalDeleteChildBatch of them, and the machine row only when
// none is left. Deleting an 18k-session machine in one SERIALIZABLE statement ran Postgres out of predicate locks
// (prod 2026-11-19, "out of shared memory" on DELETE FROM user_machines). Other children cascade with the machine row.
var finalDeleteChildren = []struct{ table, column string }{
	{"user_machine_access_sessions", "user_machine_id"},
	{"user_machine_terminal_session_operations", "user_machine_id"},
	{"user_machine_terminal_sessions", "user_machine_id"},
	{"preview_leases", "owner_machine_id"},
	{"inspector_credentials", "native_machine_id"},
	{"machine_control_renewals", "machine_id"},
}

const finalDeleteChildBatch = 1000

// finalDeleteMachine reports more when it removed a full batch of child rows and left the machine row for later.
func finalDeleteMachine(ctx context.Context, tx *db.Tx, writer *audit.Writer, machine dbsqlc.LockUserMachinesForFinalDeletionRow) (more bool, err error) {
	// A user's revoke or delete of another machine of the account goes first (revokeUserMachineControl holds this
	// lock across its transaction); this background batch waits, and retries if its snapshot is now stale.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,$2))`, machine.UserID, machineRemovalLockSeed); err != nil {
		return false, err
	}
	for _, child := range finalDeleteChildren {
		tag, err := tx.Exec(ctx, `DELETE FROM `+child.table+` WHERE ctid IN (SELECT ctid FROM `+child.table+` WHERE `+child.column+`=$1 LIMIT `+strconv.Itoa(finalDeleteChildBatch)+`)`, machine.ID)
		if err != nil {
			return false, fmt.Errorf("final delete removed machine's %s: %w", child.table, err)
		}
		if tag.RowsAffected() == finalDeleteChildBatch {
			return true, nil
		}
	}
	if err := writer.WriteTx(ctx, tx, audit.Event{ActorUserID: machine.UserID, ActorType: audit.ActorSystem,
		EventType: "user_machine.final_deleted", ResourceType: "user_machine", ResourceID: machine.ID,
		IdempotencyKey: "user_machine.final_deleted:" + machine.ID, Metadata: map[string]any{"alias": machine.Alias}}); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_machines WHERE id=$1`, machine.ID); err != nil {
		return false, fmt.Errorf("final delete removed machine: %w", err)
	}
	return false, nil
}

// revokeSessionCredentials marks the sessions whose helper revocation is done, many ids per statement: a signed
// credential needs no helper call, and a statement per session made the revoke of a machine with 20,000 sessions
// take 90 s.
func (s *Service) revokeSessionCredentials(ctx context.Context, rows []dbsqlc.RevokeUserMachineAccessSessionsRow) error {
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	var errs []error
	for chunk := range slices.Chunk(ids, 1000) {
		if _, err := s.db.Queries().MarkUserMachineAccessSessionsRevoked(ctx, chunk); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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
	if !supportedMachineTarget(platform, architecture) || !slices.Contains(s.policy.AllowedPlatforms, platform) {
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

// supportedMachineTarget is the set of operating systems and CPUs a machine can pair from. It is independent of the
// release matrix: a platform pairs as soon as its binary exists, whether or not the release pipeline publishes it yet.
func supportedMachineTarget(platform, architecture string) bool {
	switch platform + "/" + architecture {
	case "linux/amd64", "linux/arm64", "darwin/arm64", "windows/amd64", "windows/arm64":
		return true
	}
	return false
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

func validEnrollmentToken(token string) bool {
	token = strings.TrimSpace(token)
	if len(token) != enrollmentTokenLength {
		return false
	}
	for i := range token {
		if !((token[i] >= '0' && token[i] <= '9') || (token[i] >= 'A' && token[i] <= 'Z')) {
			return false
		}
	}
	return true
}

func enrollmentTokenHash(token string) [sha256.Size]byte {
	token = strings.TrimSpace(token)
	return sha256.Sum256([]byte(token))
}

// randomEnrollmentToken returns one URL-safe credential, the same kind for every platform: the installer path
// (the installer address picks the script by user agent) says which shell runs it, not the token.
func randomEnrollmentToken() (string, error) {
	return randomCodeFromAlphabet("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ", enrollmentTokenLength)
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
