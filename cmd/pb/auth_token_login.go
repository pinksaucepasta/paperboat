package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/prompt"
)

var authCredentialStoreAvailable = config.CredentialStoreAvailable

var readAuthEnrollmentToken = func(c *command.Context) ([]byte, error) {
	return prompt.Secret(prompt.SecretOptions{Context: c.Context, Title: "Enrollment token", Description: "Paste the 26-character token", Stdin: os.Stdin, Output: c.ErrWriter, MaxBytes: 26})
}

func authTokenLogin(c *command.Context) error {
	cfg, err := config.Load(c.String("config"))
	if err != nil {
		return err
	}
	issuer := cfg.ServerURL
	if override := strings.TrimSpace(c.String("server")); override != "" {
		issuer = override
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
	// Match fresh headless enrollment without changing the storage policy of
	// an existing account when its OS credential service is unavailable.
	if !cfg.Auth.AllowFileFallback && !authCredentialStoreAvailable() {
		if _, err := store.Load(issuer); err == nil {
			return errors.New("this CLI already has a session; run `pb auth logout` before signing in with another enrollment token")
		} else if !errors.Is(err, config.ErrNoCredentials) {
			return err
		}
		cfg.Auth.AllowFileFallback = true
		if err := cfg.Save(); err != nil {
			return fmt.Errorf("prepare protected credential storage: %w", err)
		}
		store, err = config.ProfileStoreFor(cfg)
		if err != nil {
			return err
		}
	}
	if err := store.Recover(issuer); err != nil {
		return err
	}
	if err := drainPendingRevocations(c.Context, issuer, store); err != nil {
		return errors.New("previous session revocation is pending; retry `pb auth login` when the server is reachable")
	}
	var account config.Account
	err = store.WithEnrollmentLogin(issuer, func(state *config.EnrollmentLoginState, save func() error) error {
		if state.Cancelled {
			if err := cancelPendingTokenLogin(c.Context, issuer, store, state, save); err != nil {
				return err
			}
			if err := drainPendingRevocations(c.Context, issuer, store); err != nil {
				return errors.New("previous login revocation is pending; retry when the server is reachable")
			}
		}
		if state.Token == "" {
			if _, err := store.Load(issuer); err == nil {
				return errors.New("this CLI already has a session; run `pb auth logout` before signing in with another enrollment token")
			} else if !errors.Is(err, config.ErrNoCredentials) {
				return err
			}
			token, err := authEnrollmentTokenInput(c)
			if err != nil {
				return err
			}
			defer clear(token)
			verifier := make([]byte, 32)
			if _, err := rand.Read(verifier); err != nil {
				return err
			}
			state.Token = string(token)
			state.Verifier = base64.RawURLEncoding.EncodeToString(verifier)
			clear(verifier)
			if err := save(); err != nil {
				return fmt.Errorf("retain login recovery before using the token: %w", err)
			}
		}
		if state.Profile == nil {
			if err := exchangePendingTokenLogin(c.Context, issuer, state); err != nil {
				if definitiveEnrollmentLoginRejection(err) {
					*state = config.EnrollmentLoginState{}
					if saveErr := save(); saveErr != nil {
						return fmt.Errorf("clear rejected login recovery: %w", saveErr)
					}
				}
				return safeEnrollmentLoginError(err)
			}
			if err := save(); err != nil {
				return fmt.Errorf("retain issued session; retry `pb auth login` to finish: %w", err)
			}
		}
		if err := persistTokenLoginProfile(store, *state.Profile, *state.Credential); err != nil {
			return fmt.Errorf("sign-in is incomplete; retry `pb auth login` to finish: %w", err)
		}
		if err := cfg.Save(); err != nil {
			return fmt.Errorf("session saved, but server configuration is incomplete; retry the same login command: %w", err)
		}
		account = state.Profile.Account
		*state = config.EnrollmentLoginState{}
		if err := save(); err != nil {
			return fmt.Errorf("session saved, but login recovery cleanup is incomplete; retry `pb auth login`: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, map[string]any{"signed_in": true, "account": account})
	}
	_, err = fmt.Fprintf(c.Writer, "Signed in as %s\n", firstNonEmpty(account.Email, account.DisplayName, account.ID))
	return err
}

func authEnrollmentTokenInput(c *command.Context) ([]byte, error) {
	var token []byte
	if path := strings.TrimSpace(c.String("token-file")); path != "" {
		value, err := bootstrap.ReadEnrollmentTokenFile(path)
		if err != nil {
			return nil, invocationError(errors.New("--token-file must be an absolute protected file containing the enrollment token"))
		}
		token = []byte(value)
	} else {
		if c.Bool("json") {
			return nil, invocationError(errors.New("use --token-file with --json for a new login"))
		}
		var err error
		token, err = readAuthEnrollmentToken(c)
		if err != nil {
			return nil, err
		}
	}
	valid := len(token) == 26
	for _, b := range token {
		valid = valid && ((b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9'))
	}
	if !valid {
		clear(token)
		return nil, invocationError(errors.New("enrollment token must contain exactly 26 uppercase letters or digits"))
	}
	return token, nil
}

func exchangePendingTokenLogin(ctx context.Context, issuer string, state *config.EnrollmentLoginState) error {
	label, _ := os.Hostname()
	if label == "" {
		label = "Paperboat CLI"
	}
	result, err := api.New(issuer, config.Credential{}, nil).LoginWithEnrollmentToken(ctx, state.Token, state.Verifier, label, runtime.GOOS)
	if err != nil {
		return err
	}
	credential := config.Credential{AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, TokenType: result.TokenType, ExpiresAt: time.Now().UTC().Add(time.Duration(result.ExpiresIn) * time.Second)}
	profile := config.Profile{Issuer: issuer, Account: result.Account, CLIClientSessionID: result.CLIClientSessionID, AccessExpiresAt: credential.ExpiresAt}
	state.Profile = &profile
	state.Credential = &credential
	return nil
}

func definitiveEnrollmentLoginRejection(err error) bool {
	var ae *api.APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.Code {
	case "invalid_grant", "access_denied", "expired_token", "validation_failed":
		return true
	}
	return false
}

func safeEnrollmentLoginError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("login timed out; retry `pb auth login` to resume: %w", context.DeadlineExceeded)
	}
	var ae *api.APIError
	if errors.As(err, &ae) {
		switch ae.Code {
		case "invalid_grant", "access_denied", "expired_token":
			return errors.New("enrollment token is invalid, expired, cancelled, or already used; obtain a fresh token and retry `pb auth login`")
		case "validation_failed":
			return errors.New("server rejected the enrollment login request; check the token and CLI version")
		case "rate_limited":
			return errors.New("too many login attempts; wait and retry `pb auth login`")
		}
	}
	return errors.New("could not exchange the enrollment token; retry `pb auth login` to resume safely")
}

func persistTokenLoginProfile(store config.ProfileStore, profile config.Profile, credential config.Credential) error {
	existing, err := store.Load(profile.Issuer)
	if err == nil {
		if existing.CLIClientSessionID != profile.CLIClientSessionID || existing.Account.ID != profile.Account.ID {
			return errors.New("another sign-in is active; run `pb auth logout` before continuing")
		}
		current, err := store.CredentialFor(profile.Issuer)
		if err == nil && current.AccessToken == credential.AccessToken && current.RefreshToken == credential.RefreshToken {
			return nil
		}
		return errors.New("saved session credentials do not match the pending login")
	}
	if !errors.Is(err, config.ErrNoCredentials) {
		return err
	}
	err = store.Save(profile, credential)
	if err != nil {
		// ProfileStore may report post-commit cleanup errors. Do not issue or revoke
		// another session when the exact returned pair is already authoritative.
		current, loadErr := store.Load(profile.Issuer)
		cred, credErr := store.CredentialFor(profile.Issuer)
		if loadErr == nil && credErr == nil && current.CLIClientSessionID == profile.CLIClientSessionID && cred.AccessToken == credential.AccessToken && cred.RefreshToken == credential.RefreshToken {
			return nil
		}
	}
	return err
}

func cancelPendingTokenLogin(ctx context.Context, issuer string, store config.ProfileStore, state *config.EnrollmentLoginState, save func() error) error {
	if state.Token == "" {
		return nil
	}
	state.Cancelled = true
	if err := save(); err != nil {
		return err
	}
	if state.Profile == nil {
		if err := exchangePendingTokenLogin(ctx, issuer, state); err != nil {
			if !definitiveEnrollmentLoginRejection(err) {
				return errors.New("previous login cancellation is pending; retry when the server is reachable")
			}
			*state = config.EnrollmentLoginState{}
			return save()
		}
	}
	if err := store.QueueRevocation(issuer, state.Profile.CLIClientSessionID, state.Credential.RefreshToken, state.Profile.Account.ID); err != nil {
		return err
	}
	*state = config.EnrollmentLoginState{}
	return save()
}
