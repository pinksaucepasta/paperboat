//go:build darwin || linux

package localdaemon

import (
	"context"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

type ManagedSSHConfig struct {
	ServerURL            string
	Auth                 config.AuthSource
	Store                config.ProfileStore
	CLIClientSessionID   string
	Home                 string
	RuntimeDirectory     string
	Executable           string
	OwnerUID             uint32
	InheritedAgentSocket string
}

type ManagedSSHRuntime struct {
	agent   *managedssh.AgentService
	mu      sync.Mutex
	refresh func(context.Context, []api.UserMachine) error
}

// StartManagedSSH registers the key, starts the agent and installs the OpenSSH configuration for
// machines, the inventory's list; it never lists machines itself.
func StartManagedSSH(ctx context.Context, cfg ManagedSSHConfig, machines []api.UserMachine) (*ManagedSSHRuntime, error) {
	if ctx == nil || strings.TrimSpace(cfg.ServerURL) == "" || cfg.Auth == nil || cfg.Store.Path == "" || strings.TrimSpace(cfg.CLIClientSessionID) == "" || !filepath.IsAbs(cfg.Home) || !filepath.IsAbs(cfg.RuntimeDirectory) || !filepath.IsAbs(cfg.Executable) {
		return nil, ErrInvalidInventoryConfig
	}
	identity, err := cfg.Store.ManagedSSHIdentity(cfg.ServerURL, cfg.CLIClientSessionID)
	if err != nil {
		return nil, err
	}
	credential, err := cfg.Auth.Credential()
	if err != nil {
		return nil, err
	}
	registerCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	client := api.New(cfg.ServerURL, credential, nil)
	client.SetCredentialSource(cfg.Auth)
	_, err = client.RegisterManagedSSHClientKey(registerCtx, identity.PublicKey, identity.Fingerprint, "managed-ssh-register-"+hex.EncodeToString(identity.Fingerprint[:16]))
	cancel()
	if err != nil {
		return nil, managedSSHAuthorityError(err)
	}
	capabilities, err := managedssh.ProbeOpenSSH(ctx, "ssh", 5*time.Second)
	if err != nil || !capabilities.Ready() {
		return nil, errors.Join(managedssh.ErrOpenSSHUnavailable, err)
	}
	agent, err := managedssh.StartAgentService(ctx, managedssh.AgentServiceConfig{RuntimeDirectory: cfg.RuntimeDirectory, InheritedAgentSocket: cfg.InheritedAgentSocket, Signer: identity.Signer, MaxConnections: 32, IdleTimeout: 2 * time.Minute, DelegateTimeout: 3 * time.Second})
	if err != nil {
		return nil, err
	}
	if err := managedssh.InstallManagedIdentityPublicKey(cfg.Home, cfg.OwnerUID, identity.PublicKey); err != nil {
		return nil, errors.Join(err, agent.Close())
	}
	command := strconv.Quote(cfg.Executable)
	install := func(refreshCtx context.Context, machines []api.UserMachine) error {
		// The alias domain comes from discovery and may change; it is one small request.
		configuration, err := client.ClientConfiguration(refreshCtx)
		if err != nil {
			return err
		}
		_, err = managedssh.InstallOpenSSHConfig(managedssh.OpenSSHConfig{
			Home: cfg.Home, OwnerUID: cfg.OwnerUID, AliasDomain: configuration.LocalAccessBaseDomain,
			ProxyCommand:      managedSSHProxyCommand(command),
			KnownHostsCommand: command + " __ssh-known-hosts --host %h --port %p",
			AgentSocket:       agent.Socket(),
			IdentityFile:      managedssh.ManagedIdentityPublicKeyPath(cfg.Home),
			Targets:           managedSSHAliasTargets(machines),
		})
		return err
	}
	// ctx is the daemon's lifetime: the agent above serves on it. The first configuration install
	// asks the server, so it gets its own bound.
	installCtx, cancelInstall := context.WithTimeout(ctx, managedSSHInstallTimeout)
	err = install(installCtx, machines)
	cancelInstall()
	if err != nil {
		return nil, errors.Join(err, agent.Close())
	}
	return &ManagedSSHRuntime{agent: agent, refresh: install}, nil
}

// Refresh rewrites the managed aliases for machines, the inventory's latest list.
func (r *ManagedSSHRuntime) Refresh(ctx context.Context, machines []api.UserMachine) error {
	if r == nil || r.refresh == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refresh(ctx, machines)
}

func (r *ManagedSSHRuntime) Close() error {
	if r == nil || r.agent == nil {
		return nil
	}
	return r.agent.Close()
}

func ManagedSSHHealthCode(err error) string {
	return managedSSHHealthCode(err)
}
