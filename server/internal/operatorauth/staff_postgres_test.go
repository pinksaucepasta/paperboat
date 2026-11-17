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
)

func staffTestService(t *testing.T) *Service {
	t.Helper()
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
	if err = db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	return &Service{Store: store, SecretKey: "test-encryption-key-for-operator-staff"}
}

func invite(t *testing.T, s *Service, email, role string) Invitation {
	t.Helper()
	var invitation Invitation
	err := s.Store.InTx(context.Background(), func(ctx context.Context, tx *db.Tx) (err error) {
		invitation, err = InviteTx(ctx, tx, email, role)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.operator_accounts WHERE id=$1`, invitation.OperatorID)
	})
	return invitation
}

// An invited operator: Discover reveals nothing about the invitation, the password cannot sign in, the wrong or an
// expired token is refused, the right one sets the password once and gives a pending session that must enrol.
func TestInvitedOperatorSetupIsSingleUse(t *testing.T) {
	s := staffTestService(t)
	ctx := context.Background()
	email := fmt.Sprintf("invited-%d@example.test", time.Now().UnixNano())
	invitation := invite(t, s, email, "support")
	if step, err := s.Discover(ctx, email); err != nil || step != "password" {
		t.Fatalf("discover = %q %v (an invitation must not be visible before sign-in)", step, err)
	}
	const password = "a sufficiently long operator password"
	if _, err := s.Login(ctx, email, "!"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("the unset-password marker signed in: %v", err)
	}
	if _, err := s.SetupInvited(ctx, email, "wrong-token", password); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong token: %v", err)
	}
	if _, err := s.SetupInvited(ctx, "other-"+email, invitation.Token, password); !errors.Is(err, ErrInvalid) {
		t.Fatalf("token for another email: %v", err)
	}
	if _, err := s.SetupInvited(ctx, email, invitation.Token, "short"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("short password: %v", err)
	}
	if _, err := s.Store.SQL().ExecContext(ctx, `UPDATE paperboat.operator_invitations SET expires_at=now()-interval '1 second' WHERE operator_id=$1`, invitation.OperatorID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetupInvited(ctx, email, invitation.Token, password); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expired token: %v", err)
	}
	fresh := invite(t, s, email, "billing")
	session, err := s.SetupInvited(ctx, email, fresh.Token, password)
	if err != nil || session.MFA != MFAEnroll || session.Account.Role != "billing" {
		t.Fatalf("setup: %+v %v", session, err)
	}
	if _, err = s.Authenticate(ctx, session.Token, "", false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a pending session must not authenticate: %v", err)
	}
	if _, err = s.SetupInvited(ctx, email, fresh.Token, password); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a token worked twice: %v", err)
	}
	if step, err := s.Discover(ctx, email); err != nil || step != "password" {
		t.Fatalf("after setup discover = %q %v", step, err)
	}
	if login, err := s.Login(ctx, email, password); err != nil || login.MFA != MFAEnroll {
		t.Fatalf("login with the chosen password: %+v %v", login, err)
	}
	// A disabled operator cannot finish an invitation.
	disabledEmail := "disabled-" + email
	disabled := invite(t, s, disabledEmail, "support")
	if _, err = s.Store.SQL().ExecContext(ctx, `UPDATE paperboat.operator_accounts SET status='disabled' WHERE id=$1`, disabled.OperatorID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetupInvited(ctx, disabledEmail, disabled.Token, password); !errors.Is(err, ErrInvalid) {
		t.Fatalf("disabled operator finished setup: %v", err)
	}
}

// Disabling the only active admin is refused even when the actor is not that admin (two admins disabling each
// other at once reach this check one after the other under the admins lock).
func TestLastActiveAdminCannotBeDisabled(t *testing.T) {
	s := staffTestService(t)
	ctx := context.Background()
	var disabledHere []string
	rows, err := s.Store.SQL().QueryContext(ctx, `UPDATE paperboat.operator_accounts SET status='disabled' WHERE role='admin' AND status='active' RETURNING id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		disabledHere = append(disabledHere, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("op_last_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		s.Store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.operator_accounts WHERE id=$1`, id)
		s.Store.SQL().ExecContext(context.Background(), `UPDATE paperboat.operator_accounts SET status='active' WHERE id=ANY($1)`, disabledHere)
	})
	if _, err := s.Store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_accounts(id,email,role,password_hash,status) VALUES($1,$1||'@test.invalid','admin','x','active')`, id); err != nil {
		t.Fatal(err)
	}
	err = s.Store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		_, err := SetDisabledTx(ctx, tx, "operator_someone_else", id, true)
		return err
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("disabling the last active admin: %v", err)
	}
}

// A lockout from failed second-factor codes ends with an MFA reset: the operator can sign in at once and enrol.
func TestMFAResetClearsLockoutAndLetsTheOperatorEnrol(t *testing.T) {
	s := staffTestService(t)
	ctx := context.Background()
	email := fmt.Sprintf("locked-%d@example.test", time.Now().UnixNano())
	const password = "a sufficiently long operator password"
	invitation := invite(t, s, email, "support")
	if _, err := s.SetupInvited(ctx, email, invitation.Token, password); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.SQL().ExecContext(ctx, `UPDATE paperboat.operator_accounts SET locked_until=now()+interval '15 minutes' WHERE id=$1`, invitation.OperatorID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, email, password); !errors.Is(err, ErrLocked) {
		t.Fatalf("a locked operator signed in: %v", err)
	}
	err := s.Store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		return ResetMFATx(ctx, tx, "operator_someone_else", invitation.OperatorID)
	})
	if err != nil {
		t.Fatal(err)
	}
	if login, err := s.Login(ctx, email, password); err != nil || login.MFA != MFAEnroll {
		t.Fatalf("after the reset: %+v %v", login, err)
	}
}
