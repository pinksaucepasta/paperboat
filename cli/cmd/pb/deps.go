package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	sessionauth "github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	helperconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/telemetry"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

func backendClient(c *command.Context) (*api.Client, error) {
	d, err := buildDeps(c)
	if err != nil {
		return nil, err
	}
	if d.cfg.ServerURL == "" {
		return nil, commandRejection{reason: commandRejectMissingServer}
	}
	client, err := newWorkspaceAPIClient(c, d.cfg.ServerURL, d.auth)
	if err != nil {
		return nil, err
	}
	return client, nil
}

// deps bundles production dependencies for a command.
type deps struct {
	cfg              *config.Config
	auth             config.AuthSource
	resolver         resolver.MachineResolver
	tunnel           tunnel.Tunnel
	peerLocal        *localapi.Client
	peerApplications peerApplicationTunnel
	telemetry        telemetry.Sink
}

type peerApplicationTunnel interface {
	Dial(context.Context, resolver.ConnectInfo) (tunnel.Conn, error)
	DialExec(context.Context, resolver.ConnectInfo, tunnel.ExecRequest) (tunnel.ExecConn, error)
	DialSSH(context.Context, resolver.ConnectInfo, string) (tunnel.Conn, error)
}

func buildDeps(c *command.Context) (*deps, error) {
	cfg, err := config.Load(c.String("config"))
	if err != nil {
		return nil, err
	}
	if s := c.String("server"); s != "" {
		normalized, err := config.NormalizeServerURL(s)
		if err != nil {
			return nil, err
		}
		cfg.ServerURL = normalized
	} else if cfg.ServerURL != "" {
		normalized, err := config.NormalizeServerURL(cfg.ServerURL)
		if err != nil {
			return nil, fmt.Errorf("invalid configured Paperboat server: %w", err)
		}
		cfg.ServerURL = normalized
	}
	var authSource config.AuthSource = config.NoCredentialsSource{}
	if cfg.ServerURL != "" {
		authSource, err = sessionauth.NewSource(cfg)
		if err != nil {
			return nil, err
		}
	}
	var selectedTunnel tunnel.Tunnel
	var peerApplications peerApplicationTunnel
	var peerLocal *localapi.Client
	if cfg.ServerURL != "" {
		paths, pathsErr := currentLocalDaemonPaths()
		if pathsErr != nil {
			return nil, pathsErr
		}
		localClient, clientErr := localapi.NewClient(paths.SocketPath, time.Duration(config.PeerConnectTimeoutMilliseconds)*time.Millisecond)
		if clientErr != nil {
			return nil, clientErr
		}
		localPeer := &tunnel.LocalPeerTunnel{Client: localClient}
		peerApplications = localPeer
		peerLocal = localClient
		selectedTunnel = localPeer
	}
	return &deps{
		cfg:              cfg,
		auth:             authSource,
		resolver:         nil,
		tunnel:           selectedTunnel,
		peerLocal:        peerLocal,
		peerApplications: peerApplications,
	}, nil
}

func runtimeIdentityStore() (*identity.Store, error) {
	stateRoot := os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT")
	var err error
	if stateRoot == "" {
		stateRoot, err = helperconfig.DefaultStateRoot(os.Getenv)
		if err != nil {
			return nil, err
		}
	}
	return identity.Open(identity.Config{StateRoot: stateRoot})
}

func configuredMachineID() (string, error) {
	store, err := runtimeIdentityStore()
	if err != nil {
		return "", err
	}
	registration, err := store.Registration()
	if errors.Is(err, os.ErrNotExist) {
		return "", commandPreparationFailure{step: prepareMachineNotAdded, cause: err}
	}
	if err != nil || registration.MachineID == "" {
		return "", commandPreparationFailure{step: prepareMachineRegistration, cause: err}
	}
	return registration.MachineID, nil
}
