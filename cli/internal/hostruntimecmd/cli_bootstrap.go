package hostruntimecmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
)

// installBootstrapCLI initializes the enrollment's CLI profile, endpoint
// identity, and daemon before its durable machine runtime is installed.
func installBootstrapCLI(ctx context.Context, session *bootstrap.ClientSession, serverURL string) error {
	if session == nil || session.Schema != "paperboat.cli-session/v1" || session.SessionID == "" || session.AccessToken == "" || session.RefreshToken == "" || session.ExpiresIn <= 0 {
		return errors.New("server returned invalid CLI session")
	}
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	cfg.ServerURL = strings.TrimRight(serverURL, "/")
	// Dashboard bootstrap is non-interactive. On headless Linux there is no
	// Secret Service session, so select the owner-only file store before any
	// profile or E2EE material is written. Desktop sessions keep using the OS
	// credential store; this does not weaken their storage policy.
	if !cfg.Auth.AllowFileFallback && !config.CredentialStoreAvailable() {
		cfg.Auth.AllowFileFallback = true
		if err := cfg.Save(); err != nil {
			return fmt.Errorf("enable protected file credential storage: %w", err)
		}
	}
	store, err := config.ProfileStoreFor(cfg)
	if err != nil {
		return err
	}
	cred := config.Credential{AccessToken: session.AccessToken, RefreshToken: session.RefreshToken, TokenType: session.TokenType, ExpiresAt: time.Now().UTC().Add(time.Duration(session.ExpiresIn) * time.Second)}
	client := api.New(cfg.ServerURL, cred, nil)
	return installBootstrapCLIWith(ctx, session, cfg.ServerURL, store, cred, client, bootstrapLocalDaemonInstaller(cfg))
}

func installBootstrapCLIWith(ctx context.Context, session *bootstrap.ClientSession, issuer string, store config.ProfileStore, cred config.Credential, client auth.LoginClient, installDaemon func(context.Context) error) error {
	if session == nil || client == nil || installDaemon == nil {
		return errors.New("bootstrap CLI installation is not configured")
	}
	if _, err := auth.CompleteLogin(ctx, auth.LoginCompletion{Store: store, Client: client, Issuer: issuer, Credential: cred, SessionID: session.SessionID}); err != nil {
		return err
	}
	return installDaemon(ctx)
}

func shouldInstallBootstrapCLI(material bootstrap.Material) bool {
	return material.ClientSession != nil
}

func shouldInstallBootstrapRuntime(material bootstrap.Material) bool {
	// An enrolled identity installs the durable runtime and updater.
	return material.UserMachineID != "" && material.InstallationGeneration > 0
}
