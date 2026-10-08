package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/identitybootstrap"
)

type LoginClient interface {
	identitybootstrap.CLIClient
	Me(context.Context) (api.Me, error)
}

type LoginCompletion struct {
	Store             config.ProfileStore
	Client            LoginClient
	Issuer            string
	Credential        config.Credential
	SessionID         string
	ExpectedAccountID string
	// Browser approval freezes the session being replaced. Bootstrap leaves this
	// nil and may replace only a profile belonging to the validated account.
	PreviousSessionID  *string
	AllowAccountChange bool
}

// CompleteLogin enrolls the authenticated endpoint before activating its local
// profile. Approval/token recovery remains with the acquiring caller; endpoint
// enrollment and profile transactions can both be retried for the same session.
func CompleteLogin(ctx context.Context, r LoginCompletion) (config.Profile, error) {
	if ctx == nil || r.Client == nil || r.SessionID == "" || r.Credential.TokenType != "Bearer" || r.Credential.AccessToken == "" || r.Credential.RefreshToken == "" || !time.Now().Before(r.Credential.ExpiresAt) {
		return config.Profile{}, errors.New("approved login session is invalid")
	}
	issuer, err := config.NormalizeIssuer(r.Issuer)
	if err != nil {
		return config.Profile{}, err
	}
	me, err := r.Client.Me(ctx)
	if err != nil {
		return config.Profile{}, fmt.Errorf("validate CLI session: %w", err)
	}
	if strings.TrimSpace(me.ID) == "" || me.Status != "active" {
		return config.Profile{}, errors.New("the approved account is not active")
	}
	if r.ExpectedAccountID != "" && me.ID != r.ExpectedAccountID {
		return config.Profile{}, errors.New("approved account differs from the expected account")
	}
	p := config.Profile{Issuer: issuer, Account: config.Account{ID: me.ID, Email: me.Email, DisplayName: me.DisplayName}, CLIClientSessionID: r.SessionID, AccessExpiresAt: r.Credential.ExpiresAt}
	if err = r.Store.Recover(issuer); err != nil {
		return config.Profile{}, err
	}
	previous, loadErr := r.Store.Load(issuer)
	if loadErr != nil && !errors.Is(loadErr, config.ErrNoCredentials) {
		return config.Profile{}, loadErr
	}
	if loadErr == nil && previous.Account.ID != me.ID && !r.AllowAccountChange {
		return config.Profile{}, errors.New("existing Paperboat profile belongs to another account")
	}
	if r.PreviousSessionID != nil {
		if loadErr == nil && previous.CLIClientSessionID != r.SessionID && previous.CLIClientSessionID != *r.PreviousSessionID {
			return config.Profile{}, config.ErrProfileChanged
		}
		if errors.Is(loadErr, config.ErrNoCredentials) && *r.PreviousSessionID != "" {
			return config.Profile{}, loadErr
		}
	}
	if _, err = identitybootstrap.EnrollCLI(ctx, identitybootstrap.CLIRequest{Store: r.Store, Client: r.Client, Issuer: issuer, AccountID: me.ID, CLIClientSessionID: r.SessionID, Fresh: true}); err != nil {
		return config.Profile{}, fmt.Errorf("enroll CLI peer identity: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return config.Profile{}, err
	}
	if errors.Is(loadErr, config.ErrNoCredentials) {
		err = r.Store.Save(p, r.Credential)
	} else {
		err = r.Store.Switch(previous.CLIClientSessionID, p, r.Credential)
	}
	if err != nil {
		// Storage may have committed before reporting a lock/cleanup failure.
		active, loadErr := r.Store.Load(issuer)
		if loadErr != nil || active.Account.ID != p.Account.ID || active.CLIClientSessionID != p.CLIClientSessionID {
			return config.Profile{}, err
		}
		credential, loadErr := r.Store.CredentialFor(issuer)
		if loadErr != nil || credential.AccessToken != r.Credential.AccessToken || credential.RefreshToken != r.Credential.RefreshToken {
			return config.Profile{}, err
		}
	}
	return p, nil
}
