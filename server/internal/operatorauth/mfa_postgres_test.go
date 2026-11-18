package operatorauth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
)

// The second factor end to end: enrol, a pending session is useless, a code works once, recovery codes work once,
// five wrong codes lock the operator and end the pending sessions, and recovery codes can be replaced only with a
// fresh authenticator code.
func TestOperatorSecondFactorLifecycle(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated remote PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UnixNano()
	email, password := fmt.Sprintf("mfa-%d@example.test", stamp), "a sufficiently long operator password"
	encoded, err := passwordHash(password)
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("op_mfa_%d", stamp)
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_accounts(id,email,role,password_hash,status) VALUES($1,$2,'admin',$3,'active')`, id, email, encoded); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.operator_accounts WHERE id=$1`, id) // sessions, totp and codes cascade
	})
	s := &Service{Store: store, SecretKey: "test-encryption-key-for-operator-mfa"}

	// First sign-in: the password alone gives a pending session that must enrol.
	first, err := s.Login(ctx, email, password)
	if err != nil || first.MFA != MFAEnroll {
		t.Fatalf("first login: %+v %v", first, err)
	}
	if _, err = s.Authenticate(ctx, first.Token, "", false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a pending session must not authenticate: %v", err)
	}
	if _, state, err := s.AuthenticatePending(ctx, first.Token, "", false); err != nil || state != MFAEnroll {
		t.Fatalf("pending state %q %v", state, err)
	}
	if _, err = s.EnrollStart(ctx, first.Token, "wrong csrf"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("enrol needs the csrf token: %v", err)
	}
	if _, _, err = s.EnrollConfirm(ctx, first.Token, first.CSRF, "123456"); !errors.Is(err, ErrMFAState) {
		t.Fatalf("confirm before start: %v", err)
	}
	enrollment, err := s.EnrollStart(ctx, first.Token, first.CSRF)
	if err != nil || enrollment.Secret == "" || len(enrollment.URI) < 40 {
		t.Fatalf("start: %+v %v", enrollment, err)
	}
	var stored []byte
	if err = store.SQL().QueryRowContext(ctx, `SELECT secret_cipher FROM paperboat.operator_totp WHERE operator_id=$1`, id).Scan(&stored); err != nil || string(stored) == enrollment.Secret || len(stored) < 30 {
		t.Fatalf("the secret must be stored encrypted: %v", err)
	}
	if _, _, err = s.EnrollConfirm(ctx, first.Token, first.CSRF, "000000"); !errors.Is(err, ErrMFAInvalid) {
		t.Fatalf("wrong confirm code: %v", err)
	}
	clock := time.Now()
	s.Now = func() time.Time { return clock }
	code, _ := TOTPCode(enrollment.Secret, clock)
	codes, _, err := s.EnrollConfirm(ctx, first.Token, first.CSRF, code)
	if err != nil || len(codes) != recoveryCodeCount {
		t.Fatalf("confirm: %d codes %v", len(codes), err)
	}
	if _, err = s.Authenticate(ctx, first.Token, first.CSRF, true); err != nil {
		t.Fatalf("a confirmed session is complete: %v", err)
	}
	var expires time.Time
	if err = store.SQL().QueryRowContext(ctx, `SELECT expires_at FROM paperboat.operator_sessions WHERE token_hash=$1`, hash(first.Token)).Scan(&expires); err != nil || time.Until(expires) < 7*time.Hour {
		t.Fatalf("the complete session lasts hours, not the pending quarter hour: %v %v", expires, err)
	}
	if _, err = s.EnrollStart(ctx, first.Token, first.CSRF); !errors.Is(err, ErrMFAState) {
		t.Fatalf("an enrolled operator cannot start enrolment again: %v", err)
	}

	// Next sign-in: verify. The code the enrolment spent is a replay; the next step's code works.
	second, err := s.Login(ctx, email, password)
	if err != nil || second.MFA != MFAVerify {
		t.Fatalf("second login: %+v %v", second, err)
	}
	if _, err = s.Verify(ctx, second.Token, second.CSRF, code); !errors.Is(err, ErrMFAInvalid) {
		t.Fatalf("replay of a used code: %v", err)
	}
	clock = clock.Add(totpPeriod * time.Second)
	next, _ := TOTPCode(enrollment.Secret, clock)
	if _, err = s.Verify(ctx, second.Token, second.CSRF, next); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if _, err = s.Authenticate(ctx, second.Token, "", false); err != nil {
		t.Fatalf("verified session: %v", err)
	}
	if _, err = s.Verify(ctx, second.Token, second.CSRF, next); !errors.Is(err, ErrMFAState) {
		t.Fatalf("a complete session has nothing to verify: %v", err)
	}

	// A recovery code opens a pending session once.
	third, _ := s.Login(ctx, email, password)
	if _, err = s.Verify(ctx, third.Token, third.CSRF, codes[0]); err != nil {
		t.Fatalf("recovery code: %v", err)
	}
	fourth, _ := s.Login(ctx, email, password)
	if _, err = s.Verify(ctx, fourth.Token, fourth.CSRF, codes[0]); !errors.Is(err, ErrMFAInvalid) {
		t.Fatalf("a recovery code works once: %v", err)
	}

	// Recovery codes are replaced only from a complete session with a fresh authenticator code; the old ones die.
	if _, err = s.RegenerateRecoveryCodes(ctx, fourth.Token, fourth.CSRF, "000000"); !errors.Is(err, ErrMFAState) {
		t.Fatalf("regenerate from a pending session: %v", err)
	}
	if _, err = s.RegenerateRecoveryCodes(ctx, third.Token, third.CSRF, "000000"); !errors.Is(err, ErrMFAInvalid) {
		t.Fatalf("regenerate with a wrong code: %v", err)
	}
	clock = clock.Add(totpPeriod * time.Second)
	fresh, _ := TOTPCode(enrollment.Secret, clock)
	replaced, err := s.RegenerateRecoveryCodes(ctx, third.Token, third.CSRF, fresh)
	if err != nil || len(replaced) != recoveryCodeCount || replaced[0] == codes[0] {
		t.Fatalf("regenerate: %v %v", replaced, err)
	}
	sixth, _ := s.Login(ctx, email, password)
	if _, err = s.Verify(ctx, sixth.Token, sixth.CSRF, codes[1]); !errors.Is(err, ErrMFAInvalid) {
		t.Fatalf("an old recovery code must be dead after regeneration: %v", err)
	}
	if _, err = s.Verify(ctx, sixth.Token, sixth.CSRF, replaced[0]); err != nil {
		t.Fatalf("a new recovery code works: %v", err)
	}

	// Five wrong codes in a row lock the operator and end the pending sessions.
	fifth, _ := s.Login(ctx, email, password)
	for i := range maxFailures {
		if _, err = s.Verify(ctx, fifth.Token, fifth.CSRF, fmt.Sprintf("00000%d", i)); !errors.Is(err, ErrMFAInvalid) && !errors.Is(err, ErrInvalid) {
			t.Fatalf("wrong code %d: %v", i, err)
		}
	}
	if _, err = s.Login(ctx, email, password); !errors.Is(err, ErrLocked) {
		t.Fatalf("five wrong codes must lock the operator: %v", err)
	}
	if _, err = s.Verify(ctx, fifth.Token, fifth.CSRF, codes[1]); !errors.Is(err, ErrInvalid) {
		t.Fatalf("the pending session must be revoked by the lockout: %v", err)
	}
	var failed, locked int
	store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE actor_user_id=$1 AND event_type='operator.mfa.verify_failed'`, id).Scan(&failed)
	store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE actor_user_id=$1 AND event_type IN ('operator.mfa.enrolled','operator.mfa.verified','operator.mfa.recovery_used')`, id).Scan(&locked)
	if failed < maxFailures || locked != 4 {
		t.Fatalf("audit: %d failed, %d successes", failed, locked)
	}
}

// A guarded action checks an authenticator code at that moment: the code works once, within one step either side of
// now, never twice and never from an earlier step, a recovery code is not accepted, and wrong codes count against
// the operator's lockout.
func TestCheckTOTPSpendsACurrentCodeOnce(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated remote PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("op_check_%d", time.Now().UnixNano())
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_accounts(id,email,role,password_hash,status) VALUES($1,$1||'@example.test','admin','unused','active')`, id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.operator_accounts WHERE id=$1`, id)
	})
	const secret = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	key := "test-encryption-key-for-operator-mfa"
	sealed, err := secrets.Encrypt(key, secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_totp(operator_id,secret_cipher,confirmed_at) VALUES($1,$2,now())`, id, sealed); err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	s := &Service{Store: store, SecretKey: key, Now: func() time.Time { return clock }}
	code := func(at time.Time) string {
		c, err := TOTPCode(secret, at)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if err = s.CheckTOTP(ctx, id, code(clock)); err != nil {
		t.Fatalf("a current code: %v", err)
	}
	if err = s.CheckTOTP(ctx, id, code(clock)); !errors.Is(err, ErrMFAInvalid) {
		t.Fatalf("the same code twice: %v", err)
	}
	if err = s.CheckTOTP(ctx, id, code(clock.Add(-totpPeriod*time.Second))); !errors.Is(err, ErrMFAInvalid) {
		t.Fatalf("a code of an earlier step than the spent one: %v", err)
	}
	if err = s.CheckTOTP(ctx, id, code(clock.Add(totpPeriod*time.Second))); err != nil {
		t.Fatalf("the next step's code is inside the tolerance: %v", err)
	}
	if err = s.CheckTOTP(ctx, id, code(clock.Add(10*totpPeriod*time.Second))); !errors.Is(err, ErrMFAInvalid) {
		t.Fatalf("a code far in the future: %v", err)
	}
	if err = s.CheckTOTP(ctx, id, "abcd-efgh-ijkl-mnop"); !errors.Is(err, ErrMFAInvalid) {
		t.Fatalf("a recovery-code shaped value: %v", err)
	}
	var failures int
	if err = store.SQL().QueryRowContext(ctx, `SELECT failed_attempts FROM paperboat.operator_accounts WHERE id=$1`, id).Scan(&failures); err != nil || failures != 3 {
		t.Fatalf("wrong codes must count against the lockout: %d %v", failures, err)
	}
}
