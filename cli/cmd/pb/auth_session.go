package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	sessionauth "github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/shared/peertransport/endpointidentity"
)

func requireAuthConfig(c *command.Context) (*config.Config, config.ProfileStore, error) {
	d, err := buildDeps(c)
	if err != nil {
		return nil, config.ProfileStore{}, err
	}
	if strings.TrimSpace(d.cfg.ServerURL) == "" {
		return nil, config.ProfileStore{}, commandRejection{reason: commandRejectMissingServer}
	}
	s, err := config.ProfileStoreFor(d.cfg)
	if err != nil {
		return nil, config.ProfileStore{}, err
	}
	return d.cfg, s, nil
}

func e2eeClient(c *command.Context) (*api.Client, config.ProfileStore, config.Profile, error) {
	cfg, err := config.Load(c.String("config"))
	if err != nil {
		return nil, config.ProfileStore{}, config.Profile{}, err
	}
	if server := strings.TrimSpace(c.String("server")); server != "" {
		cfg.ServerURL, err = config.NormalizeServerURL(server)
		if err != nil {
			return nil, config.ProfileStore{}, config.Profile{}, err
		}
	}
	if strings.TrimSpace(cfg.ServerURL) == "" {
		return nil, config.ProfileStore{}, config.Profile{}, commandRejection{reason: commandRejectMissingServer}
	}
	store, err := config.ProfileStoreFor(cfg)
	if err != nil {
		return nil, config.ProfileStore{}, config.Profile{}, err
	}
	profile, err := store.Load(cfg.ServerURL)
	if err != nil {
		return nil, config.ProfileStore{}, config.Profile{}, err
	}
	source := (&sessionauth.Source{Store: store, Issuer: cfg.ServerURL}).WithContext(c.Context)
	client, err := newWorkspaceAPIClient(c, cfg.ServerURL, source)
	if err != nil {
		return nil, config.ProfileStore{}, config.Profile{}, err
	}
	return client, store, profile, nil
}

func authStatus(c *command.Context) error {
	cfg, store, err := requireAuthConfig(c)
	if err != nil {
		return err
	}
	if err := store.Recover(cfg.ServerURL); err != nil {
		return fmt.Errorf("recover interrupted Paperboat sign-in: %w", err)
	}
	p, err := store.Load(cfg.ServerURL)
	if onlyEnvironmentFailureLeaves(err, func(leaf error) bool { return leaf == config.ErrNoCredentials }) {
		if c.Bool("json") {
			return json.NewEncoder(c.Writer).Encode(map[string]any{"signed_in": false})
		}
		fmt.Fprintln(c.Writer, "Not signed in")
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := store.CredentialFor(cfg.ServerURL); err != nil {
		return &sessionauth.CredentialFailure{Cause: err}
	}
	if c.Bool("json") {
		document := map[string]any{"signed_in": true, "issuer": p.Issuer, "cli_client_session_id": p.CLIClientSessionID, "access_expires_at": p.AccessExpiresAt, "account": p.Account}
		if root, rootErr := store.LoadPeerAccountRootPublic(p.Issuer, p.Account.ID); rootErr == nil {
			fingerprint, fingerprintErr := endpointidentity.RootFingerprint(root)
			if fingerprintErr != nil {
				return fingerprintErr
			}
			document["trusted_root_public_key"] = base64.RawURLEncoding.EncodeToString(root)
			document["trusted_root_fingerprint"] = fingerprint
		} else if !onlyEnvironmentFailureLeaves(rootErr, func(leaf error) bool { return leaf == config.ErrSecretNotFound }) {
			return fmt.Errorf("load locally trusted identity: %w", rootErr)
		}
		return json.NewEncoder(c.Writer).Encode(document)
	}
	fmt.Fprintf(c.Writer, "Signed in as %s\nServer: %s\nSession: %s\nAccess expires: %s\n", firstNonEmpty(p.Account.Email, p.Account.DisplayName, p.Account.ID), p.Issuer, p.CLIClientSessionID, p.AccessExpiresAt.Format(time.RFC3339))
	return nil
}

func authLogout(c *command.Context) error {
	cfg, store, err := requireAuthConfig(c)
	if err != nil {
		return err
	}
	if err := store.Recover(cfg.ServerURL); err != nil {
		return fmt.Errorf("recover interrupted Paperboat sign-in: %w", err)
	}
	return store.WithBrowserLogin(cfg.ServerURL, func(state *config.BrowserLoginState, save func() error) error {
		cancellationErr := cancelBrowserLogin(c.Context, cfg.ServerURL, state, save)
		return authLogoutSessions(c, cfg, store, cancellationErr)
	})
}

func authLogoutSessions(c *command.Context, cfg *config.Config, store config.ProfileStore, cancellationErr error) error {
	credentials, err := store.TakeLogoutCredentials(cfg.ServerURL)
	partial, locallySignedOut := err.(*config.LogoutRevocationIncompleteError)
	if err != nil && (!locallySignedOut || partial == nil) {
		return fmt.Errorf("remove local Paperboat sessions: %w", err)
	}
	unconfirmed := locallySignedOut
	observe := func(cause error) {
		if cause != nil && !errorreport.HTTPAttemptObserved(cause) {
			if _, localRead := cause.(*config.LogoutRevocationIncompleteError); localRead {
				errorreport.Current().ObserveFailure(c.Context, "pb", "auth", "command", "command_failed", cause)
				return
			}
			errorreport.Current().ObserveFailure(c.Context, "pb", "auth", "control_request", "control_request_failed", cause)
		}
	}
	observe(err)
	revokeCtx, cancel := context.WithTimeout(c.Context, 2*time.Second)
	defer cancel()
	// Sequential requests share one deadline. No worker can outlive local
	// signout, and the number of historical sessions never grows concurrency.
	for _, credential := range credentials {
		if revokeCtx.Err() != nil {
			unconfirmed = true
			observe(revokeCtx.Err())
			break
		}
		if err := api.RevokeToken(revokeCtx, cfg.ServerURL, credential.RefreshToken, nil); err != nil {
			unconfirmed = true
			observe(err)
		}
	}
	if cancellationErr != nil {
		observe(cancellationErr)
	}
	status := "complete"
	if unconfirmed {
		status = "unconfirmed"
	}
	if c.Bool("json") {
		result := map[string]any{"signed_out": true, "server": cfg.ServerURL, "server_revocation": status}
		if cancellationErr != nil {
			result["browser_login_cancellation"] = "unconfirmed"
		}
		return writeCLIJSON(c.Writer, result)
	}
	if _, err := fmt.Fprintln(c.Writer, "Signed out"); err != nil {
		return err
	}
	if unconfirmed {
		if _, err := fmt.Fprintln(c.ErrWriter, "Some server sessions could not be confirmed as revoked. Local credentials were removed; manage active sessions in the Paperboat dashboard."); err != nil {
			return err
		}
	}
	if cancellationErr != nil {
		if _, err := fmt.Fprintln(c.ErrWriter, "Cancellation of an earlier browser login could not be confirmed. Check pending sign-in requests in the Paperboat dashboard."); err != nil {
			return err
		}
	}
	return nil
}

func drainPendingRevocations(ctx context.Context, issuer string, store config.ProfileStore) error {
	records, err := store.PendingRevocations(issuer)
	if err != nil {
		return err
	}
	var errs []error
	for _, record := range records {
		if record.Cancelled {
			if err := store.CompleteRevocation(record); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if record.ServerRevoked {
			if err := store.CompleteRevocation(record); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		cred, err := store.PendingRevocationCredential(record)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := api.RevokeToken(ctx, issuer, cred.RefreshToken, nil); err != nil {
			errs = append(errs, fmt.Errorf("revoke client session %s: %w", record.CLIClientSessionID, err))
			continue
		}
		record, err = store.MarkRevocationSucceeded(record)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := store.CompleteRevocation(record); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
