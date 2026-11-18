// Package breakglass is the operator action that recovers from a server breach in one guarded, staged run: it
// publishes next keys ahead, waits until machines hold them, switches signing, signs every browser, CLI, API and
// recipe session out, re-seals stored secrets under the newest encryption key, optionally raises the client update
// floor, and retires the old keys. Machines are never reinstalled: they trust the old and the new key during the
// overlap (managedssh CAPropagation, the mint trust document) and drop the old one when it is retired.
//
// The run row is the whole state. A worker (Step) advances the current stage one bounded step at a time, so a
// crashed server resumes where it stopped, and every stage is idempotent against the database. Keys held in
// server configuration (mint signing keys, the encryption key ring, session keys) are deployed ahead by the
// operator, as for any rotation (server/docs/runbooks/control-plane-credential-rotation.md); Start refuses until
// they are, and the preview says which are missing.
//
// Prior art: ~/ref/teleport lib/auth/rotate.go (a CA rotates through init, update_clients, update_servers and
// standby phases and holds old and new keys in between; this run is those phases with a pickup check between them).
package breakglass

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/clientupdate"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
)

// Phrase is typed to confirm; the reason and a current authenticator code complete the confirmation.
const Phrase = "BREAK GLASS"

var (
	ErrPhrase     = errors.New("break-glass confirmation phrase is wrong")
	ErrReason     = errors.New("break-glass reason must be 8 to 500 characters")
	ErrFloor      = errors.New("break-glass client update floor is not a valid version")
	ErrRunning    = errors.New("a break-glass run is already in progress")
	ErrNotFound   = errors.New("break-glass run not found")
	ErrNotRunning = errors.New("break-glass run is not running")
)

// NotReadyError says which deployed key sets are missing; the operator deploys them and previews again.
type NotReadyError struct{ Checks []Check }

func (e *NotReadyError) Error() string {
	var missing []string
	for _, check := range e.Checks {
		if !check.OK {
			missing = append(missing, check.Name)
		}
	}
	return "break-glass cannot start: deploy the next keys first (" + strings.Join(missing, ", ") + ")"
}

type Stage string

const (
	StagePublish Stage = "publish" // (a) next SSH CAs added beside the active ones
	StagePickup  Stage = "pickup"  // (b) wait until machines fetched them, or the timeout
	StageSwitch  Stage = "switch"  // (c) the next CAs sign, the old ones become retiring
	StageRevoke  Stage = "revoke"  // (d) every session and token ends except the acting operator's
	StageReseal  Stage = "reseal"  // stored secrets move to the newest encryption key
	StageFloor   Stage = "floor"   // the client update floor is raised (optional)
	StageRetire  Stage = "retire"  // (e) old CAs and old mint keys are removed and refused
)

var stageOrder = []Stage{StagePublish, StagePickup, StageSwitch, StageRevoke, StageReseal, StageFloor, StageRetire}

// MintKeys is what the run reads of the mint provider: the active key id and every published one.
type MintKeys interface {
	KeyIDs() (active string, published []string)
}

// TOTP spends a current authenticator code of an operator (operatorauth.Service.CheckTOTP).
type TOTP interface {
	CheckTOTP(ctx context.Context, operatorID, code string) error
}

// Floor is the client update policy (clientupdate.Service).
type Floor interface {
	Get(ctx context.Context) (clientupdate.Policy, error)
	Put(ctx context.Context, actor, key, evidence string, in clientupdate.Input) (clientupdate.Policy, error)
}

// Config wires the run to the keys and services it rotates. The waits are values so tests can shorten them.
type Config struct {
	Store *db.DB
	SSH   *managedssh.Service
	Mint  MintKeys
	Floor Floor
	TOTP  TOTP
	// EncryptionKey is the configured ring, newest first; SessionKeys the configured session keys.
	EncryptionKey string
	SessionKeys   []string
	// PickupThreshold is the share of online machines that must hold the pending CAs; PickupTimeout ends the wait
	// anyway (machines that are offline pick the keys up when they reconnect); OnlineWindow is how recently a
	// machine fetched its CA set to count as online.
	PickupThreshold float64
	PickupTimeout   time.Duration
	OnlineWindow    time.Duration
	// Batch bounds the rows one step touches.
	Batch int
	Now   func() time.Time
}

type Service struct {
	cfg   Config
	audit *audit.Writer
}

func New(cfg Config) (*Service, error) {
	if cfg.Store == nil || cfg.SSH == nil || cfg.Mint == nil || cfg.TOTP == nil || cfg.EncryptionKey == "" {
		return nil, errors.New("break-glass needs the store, managed SSH, mint keys, TOTP check and encryption key")
	}
	if !secrets.ValidRing(cfg.EncryptionKey) {
		return nil, errors.New("break-glass: the encryption key ring has an empty key")
	}
	if cfg.PickupThreshold <= 0 || cfg.PickupThreshold > 1 || cfg.PickupTimeout <= 0 || cfg.OnlineWindow <= 0 || cfg.Batch <= 0 {
		return nil, errors.New("break-glass pickup and batch settings must be positive")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{cfg: cfg, audit: audit.NewWriter(cfg.Store)}, nil
}

func (s *Service) now() time.Time { return s.cfg.Now().UTC() }

// Check is one thing the operator deploys or sees before confirming.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Preview is exactly what a run would do, with counts.
type Preview struct {
	Accounts            int            `json:"accounts_with_ssh_ca"`
	CAs                 int            `json:"ssh_cas"`
	AccountsMidRotation int            `json:"accounts_mid_rotation"`
	Machines            int            `json:"machines"`
	MachinesOnline      int            `json:"machines_expected_to_pick_up"`
	Sessions            map[string]int `json:"sessions_and_tokens_to_revoke"`
	OperatorSessions    int            `json:"other_operator_sessions_to_revoke"`
	Sealed              map[string]int `json:"sealed_rows_to_reseal"`
	MintActive          string         `json:"mint_active_key"`
	MintKeys            []string       `json:"mint_keys_published"`
	MintToWithdraw      []string       `json:"mint_keys_to_withdraw"`
	EncryptionKeys      []string       `json:"encryption_key_ids"`
	SessionKeys         []string       `json:"session_key_ids"`
	FloorNow            string         `json:"client_update_floor_now"`
	Checks              []Check        `json:"checks"`
	ThirdParty          []Provider     `json:"third_party_checklist"`
}

// Ready says every check passed.
func (p Preview) Ready() bool {
	for _, check := range p.Checks {
		if !check.OK {
			return false
		}
	}
	return true
}

func (s *Service) Preview(ctx context.Context, keepSessionHash []byte) (Preview, error) {
	p := Preview{Sessions: map[string]int{}, Sealed: map[string]int{}, ThirdParty: ThirdParty()}
	count := func(query string, args ...any) (n int, err error) {
		err = s.cfg.Store.Pool().QueryRow(ctx, query, args...).Scan(&n)
		return n, err
	}
	var err error
	if p.Accounts, err = count(`SELECT count(DISTINCT account_id) FROM paperboat.managed_ssh_user_cas`); err != nil {
		return p, err
	}
	if p.CAs, err = count(`SELECT count(*) FROM paperboat.managed_ssh_user_cas`); err != nil {
		return p, err
	}
	if p.AccountsMidRotation, err = count(`SELECT count(DISTINCT account_id) FROM paperboat.managed_ssh_user_cas WHERE state = 'retiring'`); err != nil {
		return p, err
	}
	if p.Machines, err = count(`SELECT count(*) FROM paperboat.user_machines WHERE revoked_at IS NULL AND deleted_at IS NULL`); err != nil {
		return p, err
	}
	if p.MachinesOnline, err = count(`SELECT count(*) FROM paperboat.managed_ssh_ca_fetches WHERE fetched_at >= $1`, s.now().Add(-s.cfg.OnlineWindow)); err != nil {
		return p, err
	}
	for _, statement := range revocations {
		if p.Sessions[statement.name], err = count(`SELECT count(*) FROM paperboat.` + statement.table + ` WHERE ` + statement.live); err != nil {
			return p, err
		}
	}
	if p.OperatorSessions, err = count(`SELECT count(*) FROM paperboat.operator_sessions WHERE revoked_at IS NULL AND expires_at > now() AND token_hash IS DISTINCT FROM $1`, keepSessionHash); err != nil {
		return p, err
	}
	for _, target := range sealedTargets {
		n, err := count(`SELECT count(*) FROM paperboat.`+target.table+` WHERE `+target.column+` IS NOT NULL AND substring(`+target.column+` from 1 for 12) <> $1`, secrets.Stamp(s.cfg.EncryptionKey))
		if err != nil {
			return p, err
		}
		if n > 0 {
			p.Sealed[target.table+"."+target.column] = n
		}
	}
	return s.keyChecks(ctx, p)
}

// keyChecks fills the key state and the checks that say the next keys are deployed.
func (s *Service) keyChecks(ctx context.Context, p Preview) (Preview, error) {
	var published []string
	p.MintActive, published = s.cfg.Mint.KeyIDs()
	p.MintKeys, p.MintToWithdraw, p.SessionKeys = published, []string{}, []string{}
	for _, id := range published {
		if id != p.MintActive {
			p.MintToWithdraw = append(p.MintToWithdraw, id)
		}
	}
	p.EncryptionKeys = secrets.KeyIDs(s.cfg.EncryptionKey)
	for _, key := range s.cfg.SessionKeys {
		p.SessionKeys = append(p.SessionKeys, secrets.KeyID([]byte(key)))
	}
	if s.cfg.Floor != nil {
		policy, err := s.cfg.Floor.Get(ctx)
		if err != nil {
			return p, err
		}
		p.FloorNow = policy.MinimumVersion
	}
	sealed := 0
	for _, n := range p.Sealed {
		sealed += n
	}
	p.Checks = []Check{
		{"mint signing keys", len(p.MintKeys) >= 2, fmt.Sprintf("%d published, active %s; deploy the new key as active beside the old one", len(p.MintKeys), p.MintActive)},
		{"session keys", len(p.SessionKeys) >= 2, fmt.Sprintf("%d configured; deploy the new key first in PAPERBOAT_SESSION_KEYS", len(p.SessionKeys))},
		{"encryption key ring", len(p.EncryptionKeys) >= 2 || sealed == 0, fmt.Sprintf("%d keys in the ring, %d sealed rows under an older key; deploy PAPERBOAT_ENCRYPTION_KEY as new,old", len(p.EncryptionKeys), sealed)},
	}
	return p, nil
}

// StartInput is the final confirmation: phrase, reason and a current authenticator code, all checked now.
type StartInput struct {
	OperatorID      string
	KeepSessionHash []byte
	Phrase          string
	Reason          string
	TOTPCode        string
	FloorVersion    string
}

// Start confirms and creates the run; the worker does the rest. The code is spent before anything else is written.
func (s *Service) Start(ctx context.Context, in StartInput) (Run, error) {
	reason := strings.TrimSpace(in.Reason)
	switch {
	case subtle.ConstantTimeCompare([]byte(strings.TrimSpace(in.Phrase)), []byte(Phrase)) != 1:
		return Run{}, ErrPhrase
	case len(reason) < 8 || len(reason) > 500:
		return Run{}, ErrReason
	}
	if in.FloorVersion != "" {
		if _, ok := clientupdate.ParseVersion(in.FloorVersion); !ok || s.cfg.Floor == nil {
			return Run{}, ErrFloor
		}
	}
	preview, err := s.Preview(ctx, in.KeepSessionHash)
	if err != nil {
		return Run{}, err
	}
	if !preview.Ready() {
		return Run{}, &NotReadyError{Checks: preview.Checks}
	}
	if err := s.cfg.TOTP.CheckTOTP(ctx, in.OperatorID, in.TOTPCode); err != nil {
		return Run{}, err
	}
	id, err := newID()
	if err != nil {
		return Run{}, err
	}
	stages := make([]StageState, len(stageOrder))
	for index, name := range stageOrder {
		stages[index] = StageState{Name: name, State: stagePending, Detail: map[string]any{}}
	}
	if in.FloorVersion == "" {
		stages[indexOf(StageFloor)].State = stageSkipped
	}
	err = s.cfg.Store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO paperboat.breakglass_runs (id, state, operator_id, reason, keep_session_hash, floor_version, preview, stages, started_at, updated_at)
			VALUES ($1, 'running', $2, $3, $4, $5, $6, $7, $8, $8)`, id, in.OperatorID, reason, in.KeepSessionHash, in.FloorVersion, jsonValue(preview), jsonValue(stages), s.now()); err != nil {
			if isUnique(err) {
				return ErrRunning
			}
			return err
		}
		return s.writeAudit(ctx, tx, id, in.OperatorID, "breakglass.started", map[string]any{"reason": reason, "floor_version": in.FloorVersion, "preview": preview})
	})
	if err != nil {
		return Run{}, err
	}
	return s.Get(ctx, id)
}

func newID() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "bg_" + hex.EncodeToString(raw[:]), nil
}

func indexOf(stage Stage) int {
	for index, name := range stageOrder {
		if name == stage {
			return index
		}
	}
	return -1
}

// Cancel stops a run. Every stage is idempotent, so what was done stays done and a new run completes the rest.
func (s *Service) Cancel(ctx context.Context, id, operatorID string) error {
	return s.cfg.Store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE paperboat.breakglass_runs SET state = 'cancelled', finished_at = $2, updated_at = $2 WHERE id = $1 AND state = 'running'`, id, s.now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotRunning
		}
		return s.writeAudit(ctx, tx, id, operatorID, "breakglass.cancelled", nil)
	})
}

func (s *Service) writeAudit(ctx context.Context, tx *db.Tx, runID, operatorID, event string, metadata map[string]any) error {
	return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: operatorID, ActorType: audit.ActorAdmin, EventType: event, ResourceType: "breakglass_run", ResourceID: runID, Metadata: metadata})
}
