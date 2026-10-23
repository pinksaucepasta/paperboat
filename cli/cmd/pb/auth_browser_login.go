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
	"github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/pinksaucepasta/paperboat/internal/serverclock"
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

// authBrowserLogin adds this computer to the account, or signs it in again when it is already added.
// "Start fresh" removes the old local setup and machine record first, then adds the computer again.
func authBrowserLogin(c *command.Context) error {
	if path := strings.TrimSpace(c.String("api-token-file")); path != "" {
		if c.Bool("fresh") || c.Bool("reauth") || c.Bool("change-account") || c.Bool("no-browser") {
			return localArgumentError("--api-token-file signs in without the browser; it cannot be combined with --fresh, --reauth, --change-account or --no-browser")
		}
		return apiTokenLogin(c, path)
	}
	if c.Bool("fresh") {
		if err := startFresh(c); err != nil {
			return err
		}
	}
	err := browserLogin(c)
	if errors.Is(err, errStartFresh) {
		if err = startFresh(c); err != nil {
			return err
		}
		return browserLogin(c)
	}
	return err
}

func browserLogin(c *command.Context) error {
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
			if err := cancelBrowserLogin(c.Context, issuer, state, save); err != nil {
				return err
			}
		}
		old, e := store.Load(issuer)
		if e != nil && !errors.Is(e, config.ErrNoCredentials) {
			return e
		}
		if state.DeviceCode == "" {
			if e == nil && !c.Bool("reauth") && !c.Bool("change-account") {
				machineID, err := loginLocalMachineID(cfg)
				if err != nil {
					return err
				}
				if machineID == "" {
					// Signed in, but this computer is not on the account yet: add it to the same account.
					state.ExpectedAccountID = old.Account.ID
				} else if proceed, err := offerLoginChoices(c, store, issuer, old, machineID, state); err != nil || !proceed {
					return err
				}
			} else if e == nil && c.Bool("reauth") {
				state.ExpectedAccountID = old.Account.ID
			}
			label, _ := os.Hostname()
			if label == "" {
				label = "Paperboat CLI"
			}
			join, err := loginJoin(cfg, c.String("workspace"))
			if err != nil {
				return err
			}
			a, err := api.DeviceAuthorize(c.Context, issuer, label, "desktop", runtime.GOOS, join, loginHTTPClient())
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
			state.UserCode = a.UserCode
			state.Interval = a.Interval
			state.ExpiresAt = time.Now().UTC().Add(time.Duration(a.ExpiresIn) * time.Second)
			if e == nil {
				state.PreviousSessionID = old.CLIClientSessionID
			}
			if err = save(); err != nil {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(c.Context), 5*time.Second)
				cleanupErr := api.CancelDeviceLogin(cleanupCtx, issuer, a.DeviceCode, loginHTTPClient())
				cancel()
				return browserLoginFailure{message: "Pending login could not be saved; approval cleanup may be incomplete. Retry pb login or pb logout when connected.", cause: errors.Join(err, cleanupErr)}
			}
		}
		// A persisted issued response is completed before requesting or presenting another approval.
		if state.Credential == nil {
			if c.Bool("json") {
				if err := writeCLIJSON(c.Writer, map[string]any{"state": "approval_required", "approval_url": state.ApprovalURL, "expires_at": state.ExpiresAt}); err != nil {
					return err
				}
			} else {
				if _, err := fmt.Fprintf(c.ErrWriter, "Open this link on any device to sign in and approve this computer:\n\n  %s\n\nCode: %s (the page shows the same code)\nWaiting for approval…\n", state.ApprovalURL, state.UserCode); err != nil {
					return browserLoginFailure{message: "Login approval could not be displayed. Run pb login again to resume the saved approval.", cause: err}
				}
			}
			if !c.Bool("no-browser") && !c.Bool("json") {
				if err := openBrowser(state.ApprovalURL); err != nil {
					if _, writeErr := fmt.Fprintln(c.ErrWriter, "Could not open a browser. Use the link above on another device."); writeErr != nil {
						return browserLoginFailure{message: "Browser opening and fallback guidance did not finish. Run pb login again to resume the saved approval.", cause: errors.Join(err, writeErr)}
					}
					errorreport.Current().ObserveFailure(c.Context, "paperboat-cli", "login", "command", "command_failed", err)
				}
			}
			for state.Credential == nil {
				if err := c.Context.Err(); err != nil {
					return abortBrowserLogin(c.Context, issuer, state, save, err)
				}
				if !serverclock.Now().Before(state.ExpiresAt) {
					return abortBrowserLogin(c.Context, issuer, state, save, errors.New("login link expired; run `pb login` again"))
				}
				t, err := api.PollDeviceLogin(c.Context, issuer, state.DeviceCode, loginHTTPClient())
				if err == nil {
					if t.TokenType != "Bearer" || t.AccessToken == "" || t.RefreshToken == "" || t.CLIClientSessionID == "" || t.ExpiresIn <= 0 || t.ExpiresIn > 86400 {
						return abortBrowserLogin(c.Context, issuer, state, save, errors.New("server returned an invalid login session"))
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
						return abortBrowserLogin(c.Context, issuer, state, save, errors.New("the approved account is not active"))
					}
					if state.ExpectedAccountID != "" && me.ID != state.ExpectedAccountID {
						state.Credential = nil
						return abortBrowserLogin(c.Context, issuer, state, save, errors.New("approved account differs from the account being reauthenticated; use --change-account"))
					}
					if joined := t.Enrollment; joined != nil {
						state.Enrollment = &config.LoginEnrollment{BootstrapToken: joined.BootstrapToken, WorkspaceID: joined.WorkspaceID, Admission: joined.Admission, ExpiresAt: joined.ExpiresAt}
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
						return abortBrowserLogin(c.Context, issuer, state, save, browserLoginFailure{message: "login was denied or expired; run `pb login` again", cause: err})
					default:
						return fmt.Errorf("login is pending; retry `pb login`: %w", friendlyCommandError(err))
					}
				} else if c.Context.Err() != nil {
					return abortBrowserLogin(c.Context, issuer, state, save, errors.Join(err, c.Context.Err(), context.Cause(c.Context)))
				} else {
					return browserLoginFailure{message: "login connection interrupted; run `pb login` to resume the same approval", cause: err}
				}
				delay := time.Duration(state.Interval) * time.Second
				if remaining := serverclock.Until(state.ExpiresAt); remaining < delay {
					delay = remaining
				}
				if err = loginWait(c.Context, delay); err != nil {
					return abortBrowserLogin(c.Context, issuer, state, save, err)
				}
			}
		}
		// A previous attempt may have activated this session before daemon refresh
		// failed. Its durable credentials may already have rotated; never replay
		// the original approval over the active session.
		activated := e == nil && old.CLIClientSessionID == state.Profile.CLIClientSessionID && old.Account.ID == state.Profile.Account.ID
		if activated {
			if _, err = store.CredentialFor(issuer); err != nil {
				return fmt.Errorf("sign-in saved; unlock the credential store and retry `pb login`: %w", err)
			}
		} else {
			if !serverclock.Now().Before(state.Credential.ExpiresAt) {
				return abortBrowserLogin(c.Context, issuer, state, save, errors.New("approved login session expired; run `pb login` again"))
			}
			if _, err = auth.CompleteLogin(c.Context, auth.LoginCompletion{Store: store, Client: api.New(issuer, *state.Credential, loginHTTPClient()), Issuer: issuer, Credential: *state.Credential, SessionID: state.Profile.CLIClientSessionID, ExpectedAccountID: state.Profile.Account.ID, PreviousSessionID: &state.PreviousSessionID, AllowAccountChange: true}); err != nil {
				return fmt.Errorf("sign-in not activated; retry `pb login`: %w", err)
			}
		}
		if err = cfg.Save(); err != nil {
			return fmt.Errorf("sign-in saved; retry `pb login` to finish configuration: %w", err)
		}
		if err = refreshSignedInDaemon(c.Context, cfg); err != nil {
			return browserLoginFailure{message: "Sign-in is saved, but the installed daemon could not refresh. Run pb login again to finish, or pb doctor to check recovery.", cause: err}
		}
		account := state.Profile.Account
		joinedWorkspace, joinedAdmission := "", ""
		if state.Enrollment != nil {
			if err = pairLoginDevice(c, issuer, state.Enrollment, loginDeviceName()); err != nil {
				return browserLoginFailure{message: "Signed in, but this computer could not be added: " + err.Error() + ". Run pb login again to retry.", cause: err}
			}
			joinedWorkspace, joinedAdmission = state.Enrollment.WorkspaceID, state.Enrollment.Admission
		}
		*state = config.BrowserLoginState{}
		if err = save(); err != nil {
			return fmt.Errorf("signed in; retry `pb login` to clear recovery: %w", err)
		}
		if err = drainPendingRevocations(c.Context, issuer, store); err != nil {
			if !errorreport.HTTPAttemptObserved(err) {
				errorreport.Current().ObserveFailure(c.Context, "paperboat-cli", "login", "control_request", "control_request_failed", err)
			}
			if _, writeErr := fmt.Fprintln(c.ErrWriter, "Signed in. Previous session revocation is pending; `pb logout` will retry it."); writeErr != nil {
				return browserLoginFailure{message: "Sign-in succeeded, but revocation recovery guidance could not be displayed. Run pb logout to retry pending revocation.", cause: errors.Join(err, writeErr)}
			}
		}
		if c.Bool("json") {
			result := map[string]any{"signed_in": true, "account": account}
			if joinedWorkspace != "" {
				result["added_to_workspace"] = joinedWorkspace
				result["admission"] = joinedAdmission
			}
			return writeCLIJSON(c.Writer, result)
		}
		if _, err = fmt.Fprintf(c.Writer, "Signed in as %s.\n", firstNonEmpty(account.Email, account.DisplayName, account.ID)); err != nil || joinedWorkspace == "" {
			return err
		}
		added := fmt.Sprintf("Added %s to %s.", firstNonEmpty(loginDeviceName(), "this computer"), workspaceLabel(joinedWorkspace))
		if joinedAdmission == "pending" {
			added = fmt.Sprintf("Added %s. Waiting for an owner or admin of %s to accept it.", firstNonEmpty(loginDeviceName(), "this computer"), workspaceLabel(joinedWorkspace))
		}
		_, err = fmt.Fprintln(c.Writer, added)
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

type browserLoginFailure struct {
	message string
	cause   error
}

func (e browserLoginFailure) Error() string { return e.message }
func (e browserLoginFailure) Unwrap() error { return e.cause }

func cancelBrowserLogin(ctx context.Context, issuer string, s *config.BrowserLoginState, save func() error) error {
	if s.DeviceCode == "" {
		return nil
	}
	s.Cancelled = true
	if err := save(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := api.CancelDeviceLogin(ctx, issuer, s.DeviceCode, loginHTTPClient()); err != nil {
		return browserLoginFailure{message: "login cancellation is pending; retry `pb login` or `pb logout` when connected", cause: err}
	}
	*s = config.BrowserLoginState{}
	return save()
}
func abortBrowserLogin(ctx context.Context, issuer string, s *config.BrowserLoginState, save func() error, cause error) error {
	return errors.Join(cause, cancelBrowserLogin(ctx, issuer, s, save))
}
