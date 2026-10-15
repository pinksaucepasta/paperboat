package usermachines

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/environment"
)

var (
	ErrPairingResetInvalid  = errors.New("machine replacement reset input invalid")
	ErrPairingResetDenied   = errors.New("machine replacement reset authority denied")
	ErrPairingResetConflict = errors.New("machine replacement reset recovery binding differs")
)

type ResetMachine struct {
	ID                     string `json:"id"`
	PublicIdentityKey      string `json:"public_identity_key"`
	InstallationGeneration int64  `json:"installation_generation"`
	IdentitySignature      string `json:"identity_signature"`
}
type PairingResetInput struct {
	EnrollmentToken string         `json:"enrollment_token"`
	Verifier        string         `json:"verifier"`
	Platform        string         `json:"platform"`
	Architecture    string         `json:"architecture"`
	Machines        []ResetMachine `json:"machines"`
}
type PairingReset struct {
	State             string    `json:"state"`
	EnrollmentID      string    `json:"enrollment_id"`
	RetiredMachineIDs []string  `json:"retired_machine_ids"`
	CompletedAt       time.Time `json:"completed_at"`
}

func (s *Service) ResetPairing(ctx context.Context, in PairingResetInput) (PairingReset, error) {
	in, err := s.validatePairingReset(in)
	if err != nil {
		return PairingReset{}, err
	}
	payload, _ := json.Marshal(struct {
		Platform, Architecture string
		Machines               []ResetMachine
	}{in.Platform, in.Architecture, in.Machines})
	requestHash := sha256.Sum256(payload)
	verifierHash := sha256.Sum256([]byte(in.Verifier))
	tokenHash := enrollmentTokenHash(in.EnrollmentToken)
	var out PairingReset
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		// Match the deletion owner's lock order before acquiring enrollment or machine locks.
		if err := environment.LockVaultMutationsTx(ctx, tx); err != nil {
			return err
		}
		enrollment, err := tx.Queries().GetBootstrapPairingForTokenUpdate(ctx, tokenHash[:])
		if errors.Is(err, sql.ErrNoRows) {
			return ErrEnrollmentTokenInvalid
		}
		if err != nil {
			return err
		}
		switch enrollment.InstallationState {
		case "cancelled", "revoked", "deleted", "denied":
			return ErrEnrollmentTokenInvalid
		}
		var active bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND status='active')`, enrollment.BootstrapOwnerAccount.String).Scan(&active); err != nil {
			return err
		}
		if !active {
			return ErrPairingResetDenied
		}
		out = PairingReset{State: "retired", EnrollmentID: enrollment.ID}
		var oldVerifier, oldRequest, ids []byte
		err = tx.QueryRow(ctx, `SELECT verifier_hash,request_hash,retired_machine_ids,completed_at FROM user_machine_pairing_resets WHERE enrollment_id=$1 AND account_id=$2 AND enrollment_generation=$3`, enrollment.ID, enrollment.BootstrapOwnerAccount.String, enrollment.Generation).Scan(&oldVerifier, &oldRequest, &ids, &out.CompletedAt)
		if err == nil {
			if !bytes.Equal(oldVerifier, verifierHash[:]) || !bytes.Equal(oldRequest, requestHash[:]) {
				return ErrPairingResetConflict
			}
			return json.Unmarshal(ids, &out.RetiredMachineIDs)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if len(enrollment.VerifierHash) > 0 {
			return ErrEnrollmentTokenUsed
		}
		if enrollment.InstallationState == "expired" || !s.now().Before(enrollment.ExpiresAt) {
			return ErrEnrollmentTokenExpired
		}
		if enrollment.UserMachineID.Valid {
			return ErrPairingResetConflict
		}
		if enrollment.InstallationState != "awaiting_bootstrap" {
			return ErrEnrollmentTokenUsed
		}
		for _, m := range in.Machines {
			machine, err := tx.Queries().GetUserMachineForUpdate(ctx, dbsqlc.GetUserMachineForUpdateParams{ID: m.ID, UserID: enrollment.BootstrapOwnerAccount.String})
			if errors.Is(err, sql.ErrNoRows) {
				return ErrPairingResetDenied
			}
			if err != nil {
				return err
			}
			if machine.UserID != enrollment.BootstrapOwnerAccount.String || machine.PublicIdentityKey.String != m.PublicIdentityKey || machine.InstallationGeneration != m.InstallationGeneration {
				return ErrPairingResetDenied
			}
			if err := s.revokeUserMachineControlTx(ctx, tx, enrollment.BootstrapOwnerAccount.String, machine, true); err != nil {
				return err
			}
			out.RetiredMachineIDs = append(out.RetiredMachineIDs, m.ID)
		}
		out.CompletedAt = s.now().UTC().Truncate(time.Microsecond)
		ids, _ = json.Marshal(out.RetiredMachineIDs)
		if _, err := tx.Exec(ctx, `INSERT INTO user_machine_pairing_resets(enrollment_id,account_id,verifier_hash,request_hash,retired_machine_ids,completed_at,enrollment_generation) VALUES($1,$2,$3,$4,$5,$6,$7)`, enrollment.ID, enrollment.BootstrapOwnerAccount.String, verifierHash[:], requestHash[:], ids, out.CompletedAt, enrollment.Generation); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: enrollment.BootstrapOwnerAccount.String, ActorType: audit.ActorUser, EventType: "machine.replacement_retired", ResourceType: "machine_enrollment", ResourceID: enrollment.ID, IdempotencyKey: enrollment.ID + ":" + strconv.FormatInt(enrollment.Generation, 10), Metadata: map[string]any{"machine_count": len(in.Machines)}})
	})
	return out, err
}

func (s *Service) validatePairingReset(in PairingResetInput) (PairingResetInput, error) {
	in.EnrollmentToken = strings.TrimSpace(in.EnrollmentToken)
	if !validEnrollmentToken(in.EnrollmentToken) {
		return PairingResetInput{}, ErrEnrollmentTokenInvalid
	}
	in.Platform = strings.ToLower(strings.TrimSpace(in.Platform))
	in.Architecture = strings.ToLower(strings.TrimSpace(in.Architecture))
	// Use the same admission policy as pairing, without claiming an installation.
	if err := s.validatePairingPlatform(in.EnrollmentToken, in.Platform, in.Architecture); err != nil {
		return PairingResetInput{}, err
	}
	if strings.TrimSpace(in.Verifier) == "" || len(in.Verifier) < 16 || len(in.Verifier) > 256 || len(in.Machines) < 1 || len(in.Machines) > 16 {
		return PairingResetInput{}, ErrPairingResetInvalid
	}
	in.Machines = slices.Clone(in.Machines)
	slices.SortFunc(in.Machines, func(a, b ResetMachine) int { return strings.Compare(a.ID, b.ID) })
	for i, m := range in.Machines {
		key, err := base64.RawURLEncoding.Strict().DecodeString(m.PublicIdentityKey)
		if m.ID == "" || len(m.ID) > 256 || strings.TrimSpace(m.ID) != m.ID || strings.ContainsAny(m.ID, "\x00\r\n") || err != nil || len(key) != 32 || m.InstallationGeneration < 1 || i > 0 && in.Machines[i-1].ID == m.ID {
			return PairingResetInput{}, ErrPairingResetInvalid
		}
		signature, err := base64.RawURLEncoding.Strict().DecodeString(m.IdentitySignature)
		if err != nil || len(signature) != ed25519.SignatureSize {
			return PairingResetInput{}, ErrPairingResetInvalid
		}
		if !ed25519.Verify(key, resetIdentityPayload(in, m), signature) {
			return PairingResetInput{}, ErrPairingResetDenied
		}

	}
	return in, nil
}

func authorizeResetPairingTx(ctx context.Context, tx *db.Tx, enrollmentID string, generation int64, verifierHash []byte) error {
	var expected []byte
	err := tx.QueryRow(ctx, `SELECT verifier_hash FROM user_machine_pairing_resets WHERE enrollment_id=$1 AND enrollment_generation=$2`, enrollmentID, generation).Scan(&expected)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(expected, verifierHash) {
		return ErrPairingResetConflict
	}
	return nil
}

func resetIdentityPayload(in PairingResetInput, m ResetMachine) []byte {
	token := enrollmentTokenHash(in.EnrollmentToken)
	verifier := sha256.Sum256([]byte(in.Verifier))
	return []byte(strings.Join([]string{"paperboat.machine-reset/v1", hex.EncodeToString(token[:]), hex.EncodeToString(verifier[:]), in.Platform, in.Architecture, m.ID, m.PublicIdentityKey, strconv.FormatInt(m.InstallationGeneration, 10), ""}, "\n"))
}
