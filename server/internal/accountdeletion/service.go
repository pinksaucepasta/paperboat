// Package accountdeletion erases a personal Paperboat account. The deletion itself is one transaction:
// it revokes what the account controls and removes its personal data, and it keeps only what billing,
// audit and team records need, with the account anonymized. Provider cleanup (WorkOS, Stripe, the
// confirmation mail) is queued in the same transaction and runs afterwards from the outbox.
package accountdeletion

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

// ReauthenticationWindow is how recently the browser session must have signed in to delete the account.
const ReauthenticationWindow = 5 * time.Minute

var (
	// ErrReauthenticationRequired means the session signed in longer ago than ReauthenticationWindow.
	ErrReauthenticationRequired = errors.New("account deletion requires a sign-in within the last five minutes")
	// ErrConfirmationMismatch means the typed email is not the account's email.
	ErrConfirmationMismatch = errors.New("the confirmation email does not match the account email")
	// ErrAccountNotActive means the account was already deleted or is otherwise not signed in.
	ErrAccountNotActive = errors.New("the account is no longer active")
)

// TeamRef names a team by its workspace slug (its team_id) and display name.
type TeamRef struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// TeamBlock is a team the account owns while other members remain; it must be transferred or deleted first.
type TeamBlock struct {
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Members int    `json:"members"`
}

// OwnedTeamsError refuses deletion while the account owns teams with other members.
type OwnedTeamsError struct {
	Teams []TeamBlock
}

func (e *OwnedTeamsError) Error() string {
	return fmt.Sprintf("the account owns %d team(s) with other members", len(e.Teams))
}

// Service runs account deletion. The machines service supplies the same revocation the disconnect and
// delete actions use, so hosts and peers see the revocation exactly as they do for a single machine.
type Service struct {
	db       *db.DB
	machines *usermachines.Service
	mail     config.TransactionalMail
	now      func() time.Time
}

func New(store *db.DB, machines *usermachines.Service, mail config.TransactionalMail) *Service {
	return &Service{db: store, machines: machines, mail: mail, now: func() time.Time { return time.Now().UTC() }}
}

// ConsoleURL is where the account deletion page lives. The CLI and API tokens are told to open it.
func (s *Service) ConsoleURL() string {
	return strings.TrimRight(s.mail.ConsoleOrigin, "/") + "/settings/account"
}

// checkReauthentication enforces the recent sign-in rule.
func (s *Service) checkReauthentication(signedInAt time.Time) error {
	if signedInAt.IsZero() || s.now().Sub(signedInAt) > ReauthenticationWindow {
		return ErrReauthenticationRequired
	}
	return nil
}

// Delete erases the account. signedInAt is when the browser session signed in; confirmEmail must
// equal the account email. On success every session and token of the account has ended.
func (s *Service) Delete(ctx context.Context, user auth.User, signedInAt time.Time, confirmEmail string) error {
	if err := s.checkReauthentication(signedInAt); err != nil {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(confirmEmail), strings.TrimSpace(user.PrimaryEmail)) {
		return ErrConfirmationMismatch
	}
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		return s.eraseTx(ctx, tx, user.ID)
	})
}
