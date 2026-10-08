package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/selector"
)

var authCredentialStoreAvailable = config.CredentialStoreAvailable
var chooseLoginAction = selector.Choose
var loginWait = func(ctx context.Context, delay time.Duration) error {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
var loginHTTPClient = func() *http.Client {
	return &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func authBrowserLogin(c *command.Context) error {
	cfg, err := config.Load(c.String("config"))
	if err != nil {
		return err
	}
	issuer := cfg.ServerURL
	if v := strings.TrimSpace(c.String("server")); v != "" {
		issuer = v
	}
	issuer, err = config.NormalizeServerURL(issuer)
	if err != nil {
		return invocationError(err)
	}
	cfg.ServerURL = issuer
	store, err := config.ProfileStoreFor(cfg)
	if err != nil {
		return err
	}
	if !cfg.Auth.AllowFileFallback && !authCredentialStoreAvailable() {
		if _, e := store.Load(issuer); e == nil {
			return errors.New("saved credentials require the configured credential store; unlock it before signing in again")
		} else if !errors.Is(e, config.ErrNoCredentials) {
			return e
		}
		cfg.Auth.AllowFileFallback = true
		if err = cfg.Save(); err != nil {
			return err
		}
		store, err = config.ProfileStoreFor(cfg)
		if err != nil {
			return err
		}
	}
	if err = store.Recover(issuer); err != nil {
		return err
	}
	if c.Bool("reauth") && c.Bool("change-account") {
		return invocationError(errors.New("choose either --reauth or --change-account"))
	}
	return store.WithBrowserLogin(issuer, func(state *config.BrowserLoginState, save func() error) error {
		if state.Cancelled {
			if err := cancelBrowserLogin(issuer, state, save); err != nil {
				return err
			}
		}
		old, e := store.Load(issuer)
		if e != nil && !errors.Is(e, config.ErrNoCredentials) {
			return e
		}
		if state.DeviceCode == "" {
			if e == nil && !c.Bool("reauth") && !c.Bool("change-account") {
				if c.Bool("json") || !workspaceTerminal() {
					return errors.New("already signed in; use --reauth to authenticate again or --change-account to sign in as someone else")
				}
				choice, err := chooseLoginAction(selector.Options{Context: c.Context, Title: "Signed in as " + firstNonEmpty(old.Account.Email, old.Account.DisplayName, old.Account.ID), Items: []selector.Item{{ID: "keep", Title: "Keep current account"}, {ID: "reauth", Title: "Authenticate again"}, {ID: "change", Title: "Sign in with another account", Description: "Current login stays usable until the new sign-in succeeds"}}, Stdin: os.Stdin, Output: c.ErrWriter})
				if errors.Is(err, selector.ErrCanceled) {
					return nil
				}
				if err != nil {
					return err
				}
				if choice.ID == "keep" {
					return nil
				}
				if choice.ID == "reauth" {
					state.ExpectedAccountID = old.Account.ID
				}
			} else if e == nil && c.Bool("reauth") {
				state.ExpectedAccountID = old.Account.ID
			}
			label, _ := os.Hostname()
			if label == "" {
				label = "Paperboat CLI"
			}
			a, err := api.DeviceAuthorize(c.Context, issuer, label, "desktop", runtime.GOOS, loginHTTPClient())
			if err != nil {
				return friendlyCommandError(err)
			}
			if err = validateApprovalURL(a); err != nil {
				return err
			}
			state.Version = 1
			state.Issuer = issuer
			state.DeviceCode = a.DeviceCode
			state.ApprovalURL = a.VerificationURIComplete
			state.Interval = a.Interval
			state.ExpiresAt = time.Now().UTC().Add(time.Duration(a.ExpiresIn) * time.Second)
			if e == nil {
				state.PreviousSessionID = old.CLIClientSessionID
			}
			if err = save(); err != nil {
				_ = api.CancelDeviceLogin(context.WithoutCancel(c.Context), issuer, a.DeviceCode, loginHTTPClient())
				return fmt.Errorf("save pending login: %w", err)
			}
		}
		// A persisted issued response is completed before requesting or presenting another approval.
		if state.Credential == nil {
			if c.Bool("json") {
				if err := writeCLIJSON(c.Writer, map[string]any{"state": "approval_required", "approval_url": state.ApprovalURL, "expires_at": state.ExpiresAt}); err != nil {
					return err
				}
			} else {
				fmt.Fprintf(c.ErrWriter, "Open this link on any device to sign in and approve this CLI:\n\n  %s\n\nWaiting for approval…\n", state.ApprovalURL)
			}
			if !c.Bool("no-browser") && !c.Bool("json") {
				if err := openBrowser(state.ApprovalURL); err != nil {
					fmt.Fprintln(c.ErrWriter, "Could not open a browser. Use the link above on another device.")
				}
			}
			for state.Credential == nil {
				if err := c.Context.Err(); err != nil {
					return abortBrowserLogin(issuer, state, save, err)
				}
				if !time.Now().Before(state.ExpiresAt) {
					return abortBrowserLogin(issuer, state, save, errors.New("login link expired; run `pb login` again"))
				}
				t, err := api.PollDeviceLogin(c.Context, issuer, state.DeviceCode, loginHTTPClient())
				if err == nil {
					if t.TokenType != "Bearer" || t.AccessToken == "" || t.RefreshToken == "" || t.CLIClientSessionID == "" || t.ExpiresIn <= 0 || t.ExpiresIn > 86400 {
						return abortBrowserLogin(issuer, state, save, errors.New("server returned an invalid login session"))
					}
					expires := time.Now().UTC().Add(time.Duration(t.ExpiresIn) * time.Second)
					state.Credential = &config.Credential{AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, TokenType: t.TokenType, ExpiresAt: expires}
					// Never save an unchecked account; retaining the device grant recovers ambiguous responses.
					me, meErr := api.New(issuer, *state.Credential, loginHTTPClient()).Me(c.Context)
					if meErr != nil {
						state.Credential = nil
						return fmt.Errorf("approval received; run `pb login` to finish securely: %w", friendlyCommandError(meErr))
					}
					if me.ID == "" || me.Status != "active" {
						state.Credential = nil
						return abortBrowserLogin(issuer, state, save, errors.New("the approved account is not active"))
					}
					if state.ExpectedAccountID != "" && me.ID != state.ExpectedAccountID {
						state.Credential = nil
						return abortBrowserLogin(issuer, state, save, errors.New("approved account differs from the account being reauthenticated; use --change-account"))
					}
					state.Profile = &config.Profile{Issuer: issuer, Account: config.Account{ID: me.ID, Email: me.Email, DisplayName: me.DisplayName}, CLIClientSessionID: t.CLIClientSessionID, AccessExpiresAt: expires}
					if err = save(); err != nil {
						return fmt.Errorf("retain approved session; retry `pb login`: %w", err)
					}
					break
				}
				var aerr *api.APIError
				if errors.As(err, &aerr) {
					switch aerr.Code {
					case "authorization_pending":
					case "slow_down":
						state.Interval = min(60, state.Interval+5)
						if err = save(); err != nil {
							return err
						}
					case "access_denied", "expired_token", "invalid_grant", "invalid_client":
						return abortBrowserLogin(issuer, state, save, fmt.Errorf("login was denied or expired; run `pb login` again"))
					default:
						return fmt.Errorf("login is pending; retry `pb login`: %w", friendlyCommandError(err))
					}
				} else if c.Context.Err() != nil {
					return abortBrowserLogin(issuer, state, save, c.Context.Err())
				} else {
					return errors.New("login connection interrupted; run `pb login` to resume the same approval")
				}
				delay := time.Duration(state.Interval) * time.Second
				if remaining := time.Until(state.ExpiresAt); remaining < delay {
					delay = remaining
				}
				if err = loginWait(c.Context, delay); err != nil {
					return abortBrowserLogin(issuer, state, save, err)
				}
			}
		}
		if !time.Now().Before(state.Credential.ExpiresAt) {
			return abortBrowserLogin(issuer, state, save, errors.New("approved login session expired; run `pb login` again"))
		}
		if err = persistBrowserLoginProfile(store, *state); err != nil {
			return fmt.Errorf("sign-in not activated; retry `pb login`: %w", err)
		}
		if err = cfg.Save(); err != nil {
			return fmt.Errorf("sign-in saved; retry `pb login` to finish configuration: %w", err)
		}
		account := state.Profile.Account
		*state = config.BrowserLoginState{}
		if err = save(); err != nil {
			return fmt.Errorf("signed in; retry `pb login` to clear recovery: %w", err)
		}
		if err = drainPendingRevocations(c.Context, issuer, store); err != nil {
			fmt.Fprintln(c.ErrWriter, "Signed in. Previous session revocation is pending; `pb logout` will retry it.")
		}
		if c.Bool("json") {
			return writeCLIJSON(c.Writer, map[string]any{"signed_in": true, "account": account})
		}
		_, err = fmt.Fprintf(c.Writer, "Signed in as %s.\n", firstNonEmpty(account.Email, account.DisplayName, account.ID))
		return err
	})
}
func validateApprovalURL(a api.DeviceAuthorization) error {
	u, e := url.Parse(a.VerificationURIComplete)
	base, b := url.Parse(a.VerificationURI)
	if e != nil || b != nil || u.User != nil || u.Fragment != "" || u.Scheme != "https" || u.Host == "" || u.Scheme != base.Scheme || u.Host != base.Host || u.Path != base.Path || u.Query().Get("code") != a.UserCode || a.UserCode == "" || a.ExpiresIn < 1 || a.ExpiresIn > 3600 || a.Interval < 1 || a.Interval > 60 {
		return errors.New("server returned an invalid browser approval link")
	}
	return nil
}
func persistBrowserLoginProfile(store config.ProfileStore, s config.BrowserLoginState) error {
	p, e := store.Load(s.Issuer)
	if e == nil && p.CLIClientSessionID == s.Profile.CLIClientSessionID {
		cred, err := store.CredentialFor(s.Issuer)
		if err == nil && cred.AccessToken == s.Credential.AccessToken && cred.RefreshToken == s.Credential.RefreshToken {
			return nil
		}
		return errors.New("active session does not match the pending login")
	}
	if s.PreviousSessionID == "" {
		if e == nil {
			return config.ErrProfileChanged
		}
		if !errors.Is(e, config.ErrNoCredentials) {
			return e
		}
		return store.Save(*s.Profile, *s.Credential)
	}
	if e != nil {
		return e
	}
	return store.Switch(s.PreviousSessionID, *s.Profile, *s.Credential)
}
func cancelBrowserLogin(issuer string, s *config.BrowserLoginState, save func() error) error {
	if s.DeviceCode == "" {
		return nil
	}
	s.Cancelled = true
	if err := save(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := api.CancelDeviceLogin(ctx, issuer, s.DeviceCode, loginHTTPClient()); err != nil {
		return errors.New("login cancellation is pending; retry `pb login` or `pb logout` when connected")
	}
	*s = config.BrowserLoginState{}
	return save()
}
func abortBrowserLogin(issuer string, s *config.BrowserLoginState, save func() error, cause error) error {
	return errors.Join(cause, cancelBrowserLogin(issuer, s, save))
}
