package usermachines

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/environment"
	"github.com/pinksaucepasta/paperboat/server/internal/helperruntime"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/naming"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

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

func (s *Service) Rename(ctx context.Context, userID, userMachineID, alias string) (UserMachine, error) {
	userID, userMachineID, alias = strings.TrimSpace(userID), strings.TrimSpace(userMachineID), strings.TrimSpace(alias)
	if userID == "" || userMachineID == "" || invalidMachineAlias(alias) {
		return UserMachine{}, ErrInvalidMachineName
	}
	var result UserMachine
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := teams.Lock(ctx, tx); err != nil {
			return err
		}
		row, err := tx.Queries().RenameUserMachine(ctx, dbsqlc.RenameUserMachineParams{Alias: alias, ID: userMachineID, UserID: userID})
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

func (s *Service) CreateTerminalSession(ctx context.Context, userID, userMachineID, name, idempotencyKey string) (TerminalSession, error) {
	return s.createTerminalSession(ctx, userID, "", userMachineID, name, idempotencyKey, "")
}

func (s *Service) createTerminalSession(ctx context.Context, userID, sourceMachineID, userMachineID, name, idempotencyKey, cwd string) (TerminalSession, error) {
	if sourceMachineID != "" || cwd != "" {
		// An explicit local launch directory belongs to the authenticated owner
		// machine. A caller-supplied source ID never grants access to a target.
		if sourceMachineID == "" || cwd != "" && sourceMachineID != userMachineID {
			return TerminalSession{}, ErrNotFound
		}
		if allowed, err := s.sourceMachineAllowed(ctx, userID, sourceMachineID); err != nil {
			return TerminalSession{}, err
		} else if !allowed {
			return TerminalSession{}, ErrNotFound
		}
		if cwd != "" {
			// A launch directory is only for the user's own personal machine (checked again under lock).
			machine, err := s.db.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: sourceMachineID, UserID: userID})
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return TerminalSession{}, ErrNotFound
				}
				return TerminalSession{}, err
			}
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
				s.recordAccessDenied(ctx, tx.Queries(), dbsqlc.GetUserMachineForCapabilityParams{ID: userMachineID, UserID: userID, Capability: "terminal"}, false)
				return ErrNotFound
			}
			return err
		}
		lockedMachine, err := s.machineForCapability(ctx, tx.Queries(), dbsqlc.GetUserMachineForCapabilityParams{ID: userMachineID, UserID: userID, Capability: "terminal"})
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
	return s.CreateTerminalSession(ctx, userID, userMachineID, name, idempotencyKey)
}

// CreateConfiguredTerminalSessionInDirectory creates an owner-machine session at
// the invoking directory. Replays retain the original session and launch directory.
func (s *Service) CreateConfiguredTerminalSessionInDirectory(ctx context.Context, userID, sourceMachineID, userMachineID, name, idempotencyKey, cwd string) (TerminalSession, error) {
	if sourceMachineID == "" {
		return TerminalSession{}, ErrNotFound
	}
	return s.createTerminalSession(ctx, userID, sourceMachineID, userMachineID, name, idempotencyKey, cwd)
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
	// A revoked or removed machine never answers again: its pending operations end as failed instead
	// of being retried forever (prod held 133 such operations, some at 2,600 attempts).
	if _, err := s.db.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_terminal_session_operations o
 SET state='failed',completed_at=now(),updated_at=now(),last_error='machine_removed'
 FROM paperboat.user_machines m
 WHERE m.id=o.user_machine_id AND o.state='pending' AND (m.revoked_at IS NOT NULL OR m.deleted_at IS NOT NULL)`); err != nil {
		return err
	}
	// A close or delete_history on a machine that stayed unreachable past the age bound ends too: no operation stays
	// pending forever. (A creation is bounded by its attempts.) The host's orphan reconcile handles the session if
	// the machine returns.
	if age := s.policy.TerminalOperationMaxAge; age > 0 {
		if _, err := s.db.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_terminal_session_operations
 SET state='failed',completed_at=now(),updated_at=now(),last_error='machine_unreachable'
 WHERE state='pending' AND operation IN ('close','delete_history') AND created_at < now()-make_interval(secs=>$1)`, age.Seconds()); err != nil {
			return err
		}
	}
	items, err := s.db.Queries().ListDueUserMachineTerminalSessionOperations(ctx, 32)
	if err != nil {
		return err
	}
	var errs []error
	for _, item := range items {
		err := s.applyTerminalSessionOperation(ctx, item.ID, item.UserMachineID, item.TerminalSessionID, item.Operation, item.Attempts, item.UserID, item.EnvironmentID, item.ProviderRouteHttpBaseUrl)
		// A host that cannot be reached yet is not a worker failure: the operation is rescheduled with its cause in
		// last_error. Only a failure to record the outcome is.
		var rescheduled terminalOperationRescheduled
		var ended terminalOperationEnded
		if err != nil && !errors.As(err, &rescheduled) && !errors.As(err, &ended) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Service) expireClosedTerminalSessions(ctx context.Context) error {
	if _, err := s.db.Queries().DeleteFinishedUserMachineTerminalSessionOperations(ctx, dbsqlc.DeleteFinishedUserMachineTerminalSessionOperationsParams{
		CompletedBefore: sql.NullTime{Time: s.now().UTC().Add(-terminalOperationRetention), Valid: true}, BatchSize: 500,
	}); err != nil {
		return err
	}
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
		return s.terminalSessionOperationFailed(ctx, operationID, terminalSessionID, operation, attempts, errors.New("user-machine terminal control is unavailable"))
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
		if err != nil && (action == "close" || action == "delete") && helperruntime.IsSessionUnknown(err) {
			// The host has no such session: what the operation wanted is already true.
			err, observed.State = nil, "closed"
		}
		if err == nil && action == "close" && observed.State != "closed" {
			err = fmt.Errorf("helper runtime acknowledged close in state %q", observed.State)
		}
		if err == nil && action == "close" {
			err = s.db.Queries().MarkUserMachineTerminalSessionRuntimeClosed(ctx, terminalSessionID)
		}
	}
	if err != nil {
		return s.terminalSessionOperationFailed(ctx, operationID, terminalSessionID, operation, attempts, err)
	}
	return s.db.Queries().MarkUserMachineTerminalSessionOperationApplied(ctx, operationID)
}

// terminalSessionOperationFailed reschedules an attempt the host did not apply, or ends a creation that reached the
// configured attempts.
func (s *Service) terminalSessionOperationFailed(ctx context.Context, operationID, terminalSessionID, operation string, attempts int32, cause error) error {
	if operation == "create" && s.policy.TerminalCreateMaxAttempts > 0 && int(attempts)+1 >= s.policy.TerminalCreateMaxAttempts {
		return s.failTerminalSessionCreation(ctx, operationID, terminalSessionID, cause)
	}
	return s.retryTerminalSessionOperation(ctx, operationID, attempts, cause)
}

// failTerminalSessionCreation ends a creation the host never accepted: the operation is failed and the session shows
// "failed" instead of waiting forever. Close and delete keep retrying, because they become true when the host returns.
func (s *Service) failTerminalSessionCreation(ctx context.Context, operationID, terminalSessionID string, cause error) error {
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := tx.Queries().FailUserMachineTerminalSessionOperation(ctx, dbsqlc.FailUserMachineTerminalSessionOperationParams{ID: operationID, LastError: sql.NullString{String: terminalErrorSummary(ctx, cause), Valid: true}}); err != nil {
			return err
		}
		return tx.Queries().MarkUserMachineTerminalSessionRuntimeFailed(ctx, terminalSessionID)
	})
	if err != nil {
		return errors.Join(cause, err)
	}
	// The outcome is recorded (operation failed with its cause, session "failed"); a synchronous caller still gets it.
	return terminalOperationEnded{cause: fmt.Errorf("terminal creation ended after %d attempts: %w", s.policy.TerminalCreateMaxAttempts, cause)}
}

// terminalOperationEnded is the outcome of a creation the host never accepted: it is settled in the database, so it is
// not a worker failure (a host that stays away would otherwise raise one report per attempted creation).
type terminalOperationEnded struct{ cause error }

func (e terminalOperationEnded) Error() string { return e.cause.Error() }
func (e terminalOperationEnded) Unwrap() error { return e.cause }

// terminalOperationRescheduled is the outcome of an operation the host could not apply yet; it carries the cause so a
// synchronous caller can still report it.
type terminalOperationRescheduled struct{ cause error }

func (e terminalOperationRescheduled) Error() string { return e.cause.Error() }
func (e terminalOperationRescheduled) Unwrap() error { return e.cause }

func (s *Service) retryTerminalSessionOperation(ctx context.Context, id string, attempts int32, cause error) error {
	backoff := 1 << minInt(8, int(attempts)) // 1 s doubling to 256 s
	if err := s.db.Queries().RetryUserMachineTerminalSessionOperation(ctx, dbsqlc.RetryUserMachineTerminalSessionOperationParams{ID: id, RetrySeconds: float64(backoff), LastError: sql.NullString{String: terminalErrorSummary(ctx, cause), Valid: true}}); err != nil {
		return errors.Join(cause, err)
	}
	return terminalOperationRescheduled{cause: cause}
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

// DeleteMachineTx deletes a machine inside the caller's transaction with the same fences, credential
// cleanup and access-session records as Delete. Host notification stays with RetryPendingRevocations,
// which picks up the sessions this records as revoked. The caller holds the vault mutation lock
// (environment.LockVaultMutationsTx) and has read the machine with GetUserMachineForUpdate.
func (s *Service) DeleteMachineTx(ctx context.Context, tx *db.Tx, userID string, machine dbsqlc.UserMachine) error {
	return s.revokeUserMachineControlTx(ctx, tx, userID, machine, true)
}

func (s *Service) revokeUserMachineControl(ctx context.Context, userID, userMachineID string, deleted bool) error {
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
		return s.revokeUserMachineControlTx(ctx, tx, userID, machine, deleted)
	})
}

func (s *Service) revokeUserMachineControlTx(ctx context.Context, tx *db.Tx, userID string, machine dbsqlc.UserMachine, deleted bool) error {
	now := s.now().UTC()
	var err error
	var changed int64
	if deleted {
		changed, err = tx.Queries().DeleteUserMachine(ctx, dbsqlc.DeleteUserMachineParams{ID: machine.ID, UserID: userID})
	} else {
		changed, err = tx.Queries().RevokeUserMachine(ctx, dbsqlc.RevokeUserMachineParams{ID: machine.ID, UserID: userID, State: "disconnected", SeatState: "released"})
	}
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrNotFound
	}
	if machine.State != "disconnected" && machine.State != "deleted" && machine.State != "revoked" {
		if err := db.FenceRemovedMachineVaultTx(ctx, tx, machine.UserID, machine.ID); err != nil {
			return err
		}
	}
	if deleted {
		if _, err := tx.Queries().RevokeUserMachineAccessSessions(ctx, dbsqlc.RevokeUserMachineAccessSessionsParams{UserMachineID: machine.ID, Reason: sql.NullString{String: "user_machine_deleted", Valid: true}}); err != nil {
			return err
		}
	}
	// Revoking (disconnecting) a machine fences every credential it holds in the same transaction, exactly as
	// deleting it does: its CLI sessions, access and refresh tokens, E2EE keys and peer authority. Leaving the
	// pair-time CLI session alive let a revoked machine keep acting as the account until the token expired.
	if err := db.CleanupMachineMachineTx(ctx, tx, machine.UserID, machine.ID, now); err != nil {
		return err
	}
	if err := expireAuthenticatedMachineInstallationPairingsTx(ctx, tx, machine.ID, now); err != nil {
		return err
	}
	return db.RevokeMachineEnvironmentTx(ctx, tx, machine.EnvironmentID, now)
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
