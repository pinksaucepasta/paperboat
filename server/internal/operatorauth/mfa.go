package operatorauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 TOTP is defined over HMAC-SHA1, which every authenticator app implements.
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
)

// Second factor for operators: RFC 6238 TOTP (30 s steps, 6 digits, one step of clock skew either way, a code
// usable once) plus single-use recovery codes. Prior art: teleport lib/auth/password.go checkOTP (ValidateCustom with
// Period/Skew/Digits and a used-token record that rejects replays) and lib/auth/accountrecovery.go (recovery codes
// as the way back in).
const (
	totpPeriod        = 30
	totpSkew          = 1
	recoveryCodeCount = 10
	pendingLifetime   = 15 * time.Minute
	maxFailures       = 5
	lockout           = 15 * time.Minute
	totpIssuer        = "Paperboat"
)

var (
	// ErrMFAInvalid: the code is wrong, expired, already used or not a recovery code. One answer for all of them.
	ErrMFAInvalid = errors.New("invalid operator second factor")
	// ErrMFAState: the step does not fit the operator's state (enrolling when enrolled, verifying before enrolling).
	ErrMFAState = errors.New("operator second factor is not in that state")
	// ErrMFAUnavailable: no encryption key is configured, so a TOTP secret cannot be stored safely.
	ErrMFAUnavailable = errors.New("operator second factor needs the server encryption key")
)

// MFAState is what an operator's session still needs: "enroll" (no authenticator yet), "verify" (a code), or
// "verified" (complete).
type MFAState string

const (
	MFAEnroll   MFAState = "enroll"
	MFAVerify   MFAState = "verify"
	MFAVerified MFAState = "verified"
)

var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// totpAt is the RFC 4226 truncation of HMAC-SHA1(secret, step) as six digits.
func totpAt(secret []byte, step int64) string {
	mac := hmac.New(sha1.New, secret)
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := (binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff) % 1_000_000
	return leftPad(strconv.FormatUint(uint64(value), 10), 6)
}

func leftPad(s string, n int) string { return strings.Repeat("0", max(0, n-len(s))) + s }

// matchTOTP returns the time step the code proves, or 0. A step at or before lastUsed is a replay and is refused.
func matchTOTP(secret []byte, code string, now time.Time, lastUsed int64) int64 {
	if len(code) != 6 {
		return 0
	}
	current := now.Unix() / totpPeriod
	var matched int64
	for step := current - totpSkew; step <= current+totpSkew; step++ {
		// Every step is compared, so timing does not reveal which one matched.
		if subtle.ConstantTimeCompare([]byte(totpAt(secret, step)), []byte(code)) == 1 && step > lastUsed {
			matched = step
		}
	}
	return matched
}

func normalizeCode(code string) string {
	return strings.ToLower(strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(code)))
}

func isTOTPCode(code string) bool {
	if len(code) != 6 {
		return false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// newRecoveryCode is 80 random bits, shown as xxxx-xxxx-xxxx-xxxx.
func newRecoveryCode() (string, error) {
	raw := make([]byte, 10)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	s := strings.ToLower(base32NoPad.EncodeToString(raw))
	return s[0:4] + "-" + s[4:8] + "-" + s[8:12] + "-" + s[12:16], nil
}

// Enrollment is the authenticator setup shown once: the key to type in and the otpauth URI a QR code carries.
type Enrollment struct {
	Secret string `json:"secret"`
	URI    string `json:"otpauth_uri"`
}

func (s *Service) key() ([]byte, error) {
	if s.SecretKey == "" {
		return nil, ErrMFAUnavailable
	}
	return []byte(s.SecretKey), nil
}

// sessionState loads the pending session's operator and what it still needs, with the row locks `lock` asks for.
type pendingSession struct {
	operatorID, email string
	pending           bool
	enrolled          bool
}

func (s *Service) loadSession(ctx context.Context, tx *db.Tx, token, csrf string) (pendingSession, error) {
	var p pendingSession
	var csrfHash []byte
	err := tx.QueryRow(ctx, `SELECT a.id,a.email,s.mfa_pending,s.csrf_hash,EXISTS(SELECT 1 FROM paperboat.operator_totp t WHERE t.operator_id=a.id AND t.confirmed_at IS NOT NULL)
 FROM paperboat.operator_sessions s JOIN paperboat.operator_accounts a ON a.id=s.operator_id
 WHERE s.token_hash=$1 AND s.revoked_at IS NULL AND s.expires_at>now() AND a.status='active'`, hash(token)).Scan(&p.operatorID, &p.email, &p.pending, &csrfHash, &p.enrolled)
	if err != nil {
		if operatorAuthNoRowsOnly(err) {
			return p, ErrInvalid
		}
		return p, err
	}
	if csrf == "" || subtle.ConstantTimeCompare(hash(csrf), csrfHash) != 1 {
		return p, ErrInvalid
	}
	return p, nil
}

// EnrollStart creates (or replaces) the operator's unconfirmed authenticator secret. Only a pending session of an
// operator with no confirmed authenticator may do this: a signed-in operator cannot swap the factor.
func (s *Service) EnrollStart(ctx context.Context, token, csrf string) (Enrollment, error) {
	key, err := s.key()
	if err != nil {
		return Enrollment{}, err
	}
	secret := make([]byte, 20)
	if _, err := rand.Read(secret); err != nil {
		return Enrollment{}, err
	}
	cipher, err := secrets.EncryptBytes(key, []byte(base32NoPad.EncodeToString(secret)))
	if err != nil {
		return Enrollment{}, err
	}
	var out Enrollment
	err = s.Store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		p, err := s.loadSession(ctx, tx, token, csrf)
		if err != nil {
			return err
		}
		if !p.pending || p.enrolled {
			return ErrMFAState
		}
		if _, err = tx.Exec(ctx, `INSERT INTO paperboat.operator_totp(operator_id,secret_cipher) VALUES($1,$2)
 ON CONFLICT (operator_id) DO UPDATE SET secret_cipher=EXCLUDED.secret_cipher,last_used_step=0,created_at=now() WHERE paperboat.operator_totp.confirmed_at IS NULL`, p.operatorID, cipher); err != nil {
			return err
		}
		encoded := base32NoPad.EncodeToString(secret)
		out = Enrollment{Secret: encoded, URI: "otpauth://totp/" + url.PathEscape(totpIssuer+":"+p.email) + "?secret=" + encoded + "&issuer=" + url.QueryEscape(totpIssuer) + "&algorithm=SHA1&digits=6&period=30"}
		return nil
	})
	return out, err
}

// lockedSecret reads the operator's TOTP secret (with its row locked) so a code is checked and spent atomically.
func (s *Service) lockedSecret(ctx context.Context, tx *db.Tx, operatorID string, confirmed bool) (secret []byte, lastUsed int64, err error) {
	key, err := s.key()
	if err != nil {
		return nil, 0, err
	}
	var cipher []byte
	err = tx.QueryRow(ctx, `SELECT secret_cipher,last_used_step FROM paperboat.operator_totp WHERE operator_id=$1 AND (confirmed_at IS NOT NULL)=$2 FOR UPDATE`, operatorID, confirmed).Scan(&cipher, &lastUsed)
	if err != nil {
		if operatorAuthNoRowsOnly(err) {
			return nil, 0, ErrMFAState
		}
		return nil, 0, err
	}
	plain, err := secrets.DecryptBytes(key, cipher)
	if err != nil {
		return nil, 0, err
	}
	secret, err = base32NoPad.DecodeString(string(plain))
	return secret, lastUsed, err
}

// fail counts a wrong second factor against the same lockout as a wrong password: five in a row lock the operator
// for fifteen minutes and end every pending session, so guessing codes needs the password again each time.
func fail(ctx context.Context, tx *db.Tx, operatorID string, w *audit.Writer, event string) error {
	var failures int
	if err := tx.QueryRow(ctx, `UPDATE paperboat.operator_accounts SET failed_attempts=failed_attempts+1 WHERE id=$1 RETURNING failed_attempts`, operatorID).Scan(&failures); err != nil {
		return err
	}
	if failures >= maxFailures {
		if _, err := tx.Exec(ctx, `UPDATE paperboat.operator_accounts SET failed_attempts=0,locked_until=$2 WHERE id=$1`, operatorID, time.Now().Add(lockout)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE paperboat.operator_sessions SET revoked_at=now() WHERE operator_id=$1 AND mfa_pending AND revoked_at IS NULL`, operatorID); err != nil {
			return err
		}
	}
	return w.WriteTx(ctx, tx, audit.Event{ActorUserID: operatorID, ActorType: audit.ActorAdmin, EventType: event, ResourceType: "operator_mfa", ResourceID: operatorID, Metadata: map[string]any{"locked": failures >= maxFailures}})
}

// complete ends the pending state: the session becomes a full eight-hour session.
func complete(ctx context.Context, tx *db.Tx, token string) (time.Time, error) {
	expires := time.Now().UTC().Add(sessionLifetime)
	_, err := tx.Exec(ctx, `UPDATE paperboat.operator_sessions SET mfa_pending=false,expires_at=$2 WHERE token_hash=$1`, hash(token), expires)
	return expires, err
}

func newRecoveryCodes(ctx context.Context, tx *db.Tx, operatorID string) ([]string, error) {
	if _, err := tx.Exec(ctx, `DELETE FROM paperboat.operator_recovery_codes WHERE operator_id=$1`, operatorID); err != nil {
		return nil, err
	}
	codes := make([]string, 0, recoveryCodeCount)
	for range recoveryCodeCount {
		code, err := newRecoveryCode()
		if err != nil {
			return nil, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO paperboat.operator_recovery_codes(operator_id,code_hash) VALUES($1,$2)`, operatorID, hash(normalizeCode(code))); err != nil {
			return nil, err
		}
		codes = append(codes, code)
	}
	return codes, nil
}

// EnrollConfirm proves the operator's authenticator works: the code must match the unconfirmed secret. It confirms
// the factor, returns the recovery codes (shown once) and completes the session. A wrong code is returned as
// ErrMFAInvalid after being counted (the transaction commits the count, so the error is returned separately).
func (s *Service) EnrollConfirm(ctx context.Context, token, csrf, code string) (codes []string, expires time.Time, err error) {
	code = normalizeCode(code)
	var invalid bool
	err = s.Store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		p, err := s.loadSession(ctx, tx, token, csrf)
		if err != nil {
			return err
		}
		if !p.pending || p.enrolled {
			return ErrMFAState
		}
		secret, lastUsed, err := s.lockedSecret(ctx, tx, p.operatorID, false)
		if err != nil {
			return err
		}
		step := matchTOTP(secret, code, s.clock(), lastUsed)
		if step == 0 {
			invalid = true
			return fail(ctx, tx, p.operatorID, s.audit(), "operator.mfa.enroll_failed")
		}
		if _, err = tx.Exec(ctx, `UPDATE paperboat.operator_totp SET confirmed_at=now(),last_used_step=$2 WHERE operator_id=$1`, p.operatorID, step); err != nil {
			return err
		}
		if codes, err = newRecoveryCodes(ctx, tx, p.operatorID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE paperboat.operator_accounts SET failed_attempts=0 WHERE id=$1`, p.operatorID); err != nil {
			return err
		}
		if expires, err = complete(ctx, tx, token); err != nil {
			return err
		}
		return s.audit().WriteTx(ctx, tx, audit.Event{ActorUserID: p.operatorID, ActorType: audit.ActorAdmin, EventType: "operator.mfa.enrolled", ResourceType: "operator_mfa", ResourceID: p.operatorID})
	})
	if err == nil && invalid {
		err = ErrMFAInvalid
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	return codes, expires, nil
}

// Verify completes a pending session with a TOTP code or an unused recovery code.
func (s *Service) Verify(ctx context.Context, token, csrf, code string) (expires time.Time, err error) {
	code = normalizeCode(code)
	var invalid bool
	err = s.Store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		p, err := s.loadSession(ctx, tx, token, csrf)
		if err != nil {
			return err
		}
		if !p.pending || !p.enrolled {
			return ErrMFAState
		}
		event, metadata := "operator.mfa.verified", map[string]any{}
		ok := false
		if isTOTPCode(code) {
			secret, lastUsed, err := s.lockedSecret(ctx, tx, p.operatorID, true)
			if err != nil {
				return err
			}
			if step := matchTOTP(secret, code, s.clock(), lastUsed); step != 0 {
				ok = true
				if _, err = tx.Exec(ctx, `UPDATE paperboat.operator_totp SET last_used_step=$2 WHERE operator_id=$1`, p.operatorID, step); err != nil {
					return err
				}
			}
		} else if len(code) == 16 {
			tag, err := tx.Exec(ctx, `UPDATE paperboat.operator_recovery_codes SET used_at=now() WHERE operator_id=$1 AND code_hash=$2 AND used_at IS NULL`, p.operatorID, hash(code))
			if err != nil {
				return err
			}
			if ok = tag.RowsAffected() == 1; ok {
				var left int
				if err = tx.QueryRow(ctx, `SELECT count(*) FROM paperboat.operator_recovery_codes WHERE operator_id=$1 AND used_at IS NULL`, p.operatorID).Scan(&left); err != nil {
					return err
				}
				event, metadata = "operator.mfa.recovery_used", map[string]any{"codes_left": left}
			}
		}
		if !ok {
			invalid = true
			return fail(ctx, tx, p.operatorID, s.audit(), "operator.mfa.verify_failed")
		}
		if _, err = tx.Exec(ctx, `UPDATE paperboat.operator_accounts SET failed_attempts=0 WHERE id=$1`, p.operatorID); err != nil {
			return err
		}
		if expires, err = complete(ctx, tx, token); err != nil {
			return err
		}
		return s.audit().WriteTx(ctx, tx, audit.Event{ActorUserID: p.operatorID, ActorType: audit.ActorAdmin, EventType: event, ResourceType: "operator_mfa", ResourceID: p.operatorID, Metadata: metadata})
	})
	if err == nil && invalid {
		err = ErrMFAInvalid
	}
	if err != nil {
		return time.Time{}, err
	}
	return expires, nil
}

// RegenerateRecoveryCodes replaces the operator's recovery codes. It needs a complete session and a fresh TOTP code
// (not a recovery code), so a stolen session cannot mint codes without the authenticator.
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, token, csrf, code string) (codes []string, err error) {
	code = normalizeCode(code)
	var invalid bool
	err = s.Store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		p, err := s.loadSession(ctx, tx, token, csrf)
		if err != nil {
			return err
		}
		if p.pending || !p.enrolled {
			return ErrMFAState
		}
		secret, lastUsed, err := s.lockedSecret(ctx, tx, p.operatorID, true)
		if err != nil {
			return err
		}
		step := matchTOTP(secret, code, s.clock(), lastUsed)
		if step == 0 {
			invalid = true
			return fail(ctx, tx, p.operatorID, s.audit(), "operator.mfa.regenerate_failed")
		}
		if _, err = tx.Exec(ctx, `UPDATE paperboat.operator_totp SET last_used_step=$2 WHERE operator_id=$1`, p.operatorID, step); err != nil {
			return err
		}
		if codes, err = newRecoveryCodes(ctx, tx, p.operatorID); err != nil {
			return err
		}
		return s.audit().WriteTx(ctx, tx, audit.Event{ActorUserID: p.operatorID, ActorType: audit.ActorAdmin, EventType: "operator.mfa.recovery_regenerated", ResourceType: "operator_mfa", ResourceID: p.operatorID})
	})
	if err == nil && invalid {
		err = ErrMFAInvalid
	}
	return codes, err
}

func (s *Service) audit() *audit.Writer { return audit.NewWriter(s.Store) }

// TOTPCode is the code an authenticator shows for a base32 secret at a moment. The server never needs it (it only
// checks codes); tests and operator tooling use it to act as the authenticator.
func TOTPCode(secretBase32 string, at time.Time) (string, error) {
	secret, err := base32NoPad.DecodeString(strings.ToUpper(strings.TrimSpace(secretBase32)))
	if err != nil {
		return "", err
	}
	return totpAt(secret, at.Unix()/totpPeriod), nil
}
