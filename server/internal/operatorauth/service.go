package operatorauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"github.com/google/uuid"
	"reflect"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"golang.org/x/crypto/argon2"
)

var ErrInvalid = errors.New("invalid operator credentials")
var ErrLocked = errors.New("operator account temporarily locked")
var ErrBootstrapConsumed = errors.New("operator bootstrap already consumed")

const sessionLifetime = 8 * time.Hour

type Service struct {
	Store          *db.DB
	BootstrapEmail string
}
type Account struct {
	ID, Email, Role string
	AuthenticatedAt time.Time
}
type Session struct {
	Token, CSRF string
	ExpiresAt   time.Time
	Account     Account
}

type operatorAuthCleanupFailure struct {
	operation error
	cleanup   error
}

func (*operatorAuthCleanupFailure) Error() string {
	return "operator authentication transaction cleanup failed"
}

func (failure *operatorAuthCleanupFailure) Unwrap() []error {
	causes := make([]error, 0, 2)
	if failure.operation != nil {
		causes = append(causes, failure.operation)
	}
	if failure.cleanup != nil {
		causes = append(causes, failure.cleanup)
	}
	return causes
}

func withOperatorAuthRollbackFailure(operation, rollback error) error {
	if rollback == nil || rollback == sql.ErrTxDone {
		return operation
	}
	return &operatorAuthCleanupFailure{operation: operation, cleanup: rollback}
}

func captureOperatorAuthRollback(tx *sql.Tx, result *error) {
	if err := tx.Rollback(); err != nil {
		*result = withOperatorAuthRollbackFailure(*result, err)
	}
}

func canonicalEmail(v string) string { return strings.ToLower(strings.TrimSpace(v)) }
func randomToken() (string, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b), err
}
func hash(v string) []byte { h := sha256.Sum256([]byte(v)); return h[:] }
func passwordHash(password string) (string, error) {
	if len(password) < 12 || len(password) > 1024 {
		return "", ErrInvalid
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, 2, 19*1024, 1, 32)
	return "argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}
func verifyPassword(encoded, password string) bool {
	fields := strings.Split(encoded, "$")
	if len(fields) != 5 || fields[0] != "argon2id" || fields[1] != "v=19" || fields[2] != "m=19456,t=2,p=1" {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(fields[3])
	if err != nil || len(salt) != 16 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(fields[4])
	if err != nil || len(want) != 32 {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, 2, 19*1024, 1, 32)
	return subtle.ConstantTimeCompare(actual, want) == 1
}
func (s *Service) Discover(ctx context.Context, email string) (string, error) {
	email = canonicalEmail(email)
	if email == "" || len(email) > 254 || !strings.Contains(email, "@") {
		return "", ErrInvalid
	}
	var role string
	err := s.Store.SQL().QueryRowContext(ctx, `SELECT role FROM paperboat.operator_accounts WHERE email=$1 AND status='active'`, email).Scan(&role)
	if err == nil {
		return "password", nil
	}
	if !operatorAuthNoRowsOnly(err) {
		return "", err
	}
	var consumed bool
	if err = s.Store.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.operator_bootstrap)`).Scan(&consumed); err != nil {
		return "", err
	}
	if !consumed && email == canonicalEmail(s.BootstrapEmail) {
		return "setup_password", nil
	}
	return "", ErrInvalid
}
func (s *Service) Setup(ctx context.Context, email, password string) (session Session, resultErr error) {
	email = canonicalEmail(email)
	if email == "" || email != canonicalEmail(s.BootstrapEmail) {
		return Session{}, ErrInvalid
	}
	encoded, err := passwordHash(password)
	if err != nil {
		return Session{}, err
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return Session{}, err
	}
	account := Account{ID: "operator_" + id.String(), Email: email, Role: "admin"}
	token, err := randomToken()
	if err != nil {
		return Session{}, err
	}
	csrf, err := randomToken()
	if err != nil {
		return Session{}, err
	}
	now := time.Now().UTC()
	expires := now.Add(sessionLifetime)
	tx, err := db.BeginSerializable(ctx, s.Store.SQL())
	if err != nil {
		return Session{}, err
	}
	defer captureOperatorAuthRollback(tx, &resultErr)
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(543882134)`); err != nil {
		return Session{}, err
	}
	var consumed bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.operator_bootstrap)`).Scan(&consumed); err != nil {
		return Session{}, err
	}
	if consumed {
		return Session{}, ErrBootstrapConsumed
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO paperboat.operator_accounts(id,email,password_hash) VALUES($1,$2,$3)`, account.ID, email, encoded); err != nil {
		return Session{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO paperboat.operator_bootstrap(singleton,consumed_at,operator_id) VALUES(true,now(),$1)`, account.ID); err != nil {
		return Session{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO paperboat.operator_sessions(token_hash,operator_id,csrf_hash,expires_at) VALUES($1,$2,$3,$4)`, hash(token), account.ID, hash(csrf), expires); err != nil {
		return Session{}, err
	}
	if err = tx.Commit(); err != nil {
		return Session{}, err
	}
	account.AuthenticatedAt = now
	return Session{Token: token, CSRF: csrf, ExpiresAt: expires, Account: account}, nil
}
func (s *Service) Login(ctx context.Context, email, password string) (session Session, resultErr error) {
	email = canonicalEmail(email)
	// The password hash is checked with no transaction open: the row lock is held only to record
	// the result, so slow hashing never queues other sign-ins behind it (PLAN F21-14).
	var stored string
	var lockedBefore sql.NullTime
	err := s.Store.SQL().QueryRowContext(ctx, `SELECT password_hash,locked_until FROM paperboat.operator_accounts WHERE email=$1 AND status='active'`, email).Scan(&stored, &lockedBefore)
	if operatorAuthNoRowsOnly(err) {
		return Session{}, ErrInvalid
	}
	if err != nil {
		return Session{}, err
	}
	if lockedBefore.Valid && time.Now().Before(lockedBefore.Time) {
		return Session{}, ErrLocked
	}
	passwordOK := verifyPassword(stored, password)
	var account Account
	var encoded string
	var locked sql.NullTime
	var failures int
	tx, err := s.Store.SQL().BeginTx(ctx, nil)
	if err != nil {
		return Session{}, err
	}
	defer captureOperatorAuthRollback(tx, &resultErr)
	err = tx.QueryRowContext(ctx, `SELECT id,email,role,password_hash,failed_attempts,locked_until FROM paperboat.operator_accounts WHERE email=$1 AND status='active' FOR UPDATE`, email).Scan(&account.ID, &account.Email, &account.Role, &encoded, &failures, &locked)
	if operatorAuthNoRowsOnly(err) {
		return Session{}, ErrInvalid
	}
	if err != nil {
		return Session{}, err
	}
	now := time.Now().UTC()
	if locked.Valid && now.Before(locked.Time) {
		return Session{}, ErrLocked
	}
	if encoded != stored {
		return Session{}, ErrInvalid // the password changed meanwhile; this attempt checked the old one
	}
	if !passwordOK {
		failures++
		if failures >= 5 {
			_, err = tx.ExecContext(ctx, `UPDATE paperboat.operator_accounts SET failed_attempts=0,locked_until=$2 WHERE id=$1`, account.ID, now.Add(15*time.Minute))
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE paperboat.operator_accounts SET failed_attempts=$2,locked_until=NULL WHERE id=$1`, account.ID, failures)
		}
		if err != nil {
			return Session{}, err
		}
		if err = tx.Commit(); err != nil {
			return Session{}, err
		}
		return Session{}, ErrInvalid
	}
	token, err := randomToken()
	if err != nil {
		return Session{}, err
	}
	csrf, err := randomToken()
	if err != nil {
		return Session{}, err
	}
	expires := now.Add(sessionLifetime)
	if _, err = tx.ExecContext(ctx, `UPDATE paperboat.operator_accounts SET failed_attempts=0,locked_until=NULL WHERE id=$1`, account.ID); err != nil {
		return Session{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO paperboat.operator_sessions(token_hash,operator_id,csrf_hash,expires_at) VALUES($1,$2,$3,$4)`, hash(token), account.ID, hash(csrf), expires); err != nil {
		return Session{}, err
	}
	if err = tx.Commit(); err != nil {
		return Session{}, err
	}
	account.AuthenticatedAt = now
	return Session{Token: token, CSRF: csrf, ExpiresAt: expires, Account: account}, nil
}
func (s *Service) Authenticate(ctx context.Context, token, csrf string, checkCSRF bool) (Account, error) {
	if token == "" {
		return Account{}, ErrInvalid
	}
	var account Account
	var csrfHash []byte
	err := s.Store.SQL().QueryRowContext(ctx, `SELECT a.id,a.email,a.role,s.created_at,s.csrf_hash FROM paperboat.operator_sessions s JOIN paperboat.operator_accounts a ON a.id=s.operator_id WHERE s.token_hash=$1 AND s.revoked_at IS NULL AND s.expires_at>now() AND a.status='active'`, hash(token)).Scan(&account.ID, &account.Email, &account.Role, &account.AuthenticatedAt, &csrfHash)
	if operatorAuthNoRowsOnly(err) {
		return Account{}, ErrInvalid
	}
	if err != nil {
		return Account{}, err
	}
	if checkCSRF && (csrf == "" || subtle.ConstantTimeCompare(hash(csrf), csrfHash) != 1) {
		return Account{}, ErrInvalid
	}
	return account, nil
}
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	_, err := s.Store.SQL().ExecContext(ctx, `UPDATE paperboat.operator_sessions SET revoked_at=now() WHERE token_hash=$1 AND revoked_at IS NULL`, hash(token))
	return err
}

// operatorAuthNoRowsOnly keeps invalid credentials distinct from a database
// outage. A mixed or incomplete error tree cannot prove that the row was
// simply absent.
func operatorAuthNoRowsOnly(err error) bool {
	seen := make(map[error]struct{}, 8)
	visited := 0
	var walk func(error) bool
	walk = func(current error) bool {
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		if value.Comparable() {
			if _, exists := seen[current]; exists {
				return false
			}
			seen[current] = struct{}{}
		}
		visited++
		if visited > 8 {
			return false
		}
		if value.Type() == reflect.TypeOf(sql.ErrNoRows) && value.Interface() == sql.ErrNoRows {
			return true
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return false
			}
			for _, child := range children {
				if !walk(child) {
					return false
				}
			}
			return true
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			return walk(wrapped.Unwrap())
		}
		return false
	}
	return walk(err)
}
