package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/environmentmanager"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/spf13/cobra"
)

// daemonVaultCustody is only an account-vault adapter. It never uses the host
// runtime API or a file secret store. The daemon owns expiry and mutation leases.
type daemonVaultCustody struct {
	mu                                  sync.Mutex
	command                             *cobra.Command
	client                              *localapi.Client
	issuer, account, session, reference string
	generation                          uint64
	lease                               string
	unlock                              bool
}

func newDaemonVaultCustody(command *cobra.Command, profile config.Profile) (*daemonVaultCustody, error) {
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return nil, err
	}
	client, err := localapi.NewClient(paths.SocketPath, 5*time.Second)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(profile.Issuer + "\x00" + profile.Account.ID + "\x00password_vault"))
	action := command.Name()
	return &daemonVaultCustody{command: command, client: client, issuer: profile.Issuer, account: profile.Account.ID, session: profile.CLIClientSessionID,
		reference: "environment-password-vault-v1-" + hex.EncodeToString(digest[:16]), unlock: action == "unlock" || action == "init" || action == "recover"}, nil
}

func (*daemonVaultCustody) EnvironmentSecureStore() {}

func (s *daemonVaultCustody) request(ctx context.Context, operation string, value []byte) (localapi.VaultCustodyResponse, error) {
	response, err := s.client.VaultCustody(ctx, localapi.VaultCustodyRequest{Reference: s.reference, SessionID: s.session, Operation: operation, Generation: s.generation, Lease: s.lease, Value: value, Unlock: s.unlock && operation == "put"})
	if errors.Is(err, localapi.ErrVaultCustodyStale) {
		return response, environmentmanager.ErrVaultLocked
	}
	if err == nil {
		s.generation = response.Generation
	}
	return response, err
}

func (s *daemonVaultCustody) Get(reference string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if reference != s.reference {
		return "", config.ErrCredentialStoreUnavailable
	}
	response, err := s.request(s.command.Context(), "get", nil)
	defer clear(response.Value)
	if err != nil {
		return "", err
	}
	if len(response.Value) == 0 {
		return "", config.ErrSecretNotFound
	}
	return string(response.Value), nil
}

func (s *daemonVaultCustody) Set(reference, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if reference != s.reference {
		return config.ErrCredentialStoreUnavailable
	}
	raw := []byte(value)
	defer clear(raw)
	response, err := s.request(s.command.Context(), "put", raw)
	clear(response.Value)
	if err == nil {
		s.unlock = false
	}
	return err
}

func (s *daemonVaultCustody) Delete(reference string) error {
	if reference != s.reference {
		return config.ErrCredentialStoreUnavailable
	}
	return s.RemovePasswordVault(s.issuer, s.account)
}

func (s *daemonVaultCustody) LockEnvironmentMutations(issuer, account, subject string) (func() error, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if issuer != s.issuer || account != s.account || subject != "password_vault" || s.lease != "" {
		return nil, config.ErrCredentialStoreUnavailable
	}
	response, err := s.request(s.command.Context(), "begin", nil)
	if errors.Is(err, localapi.ErrTransportUnavailable) || errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		// Reuse the normal user-daemon startup owner, never a host service.
		s.client, _, err = localDaemonSnapshot(s.command, installLocalDaemonService)
		if err == nil {
			response, err = s.request(s.command.Context(), "begin", nil)
		}
	}
	if err != nil {
		return nil, err
	}
	s.lease = response.Lease
	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.WithoutCancel(s.command.Context()), 5*time.Second)
			defer cancel()
			_, releaseErr = s.request(ctx, "end", nil)
			s.lease = ""
		})
		return releaseErr
	}, nil
}

func (s *daemonVaultCustody) LockPasswordVault(issuer, account string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if issuer != s.issuer || account != s.account {
		return config.ErrCredentialStoreUnavailable
	}
	_, err := s.request(s.command.Context(), "lock", nil)
	return err
}

func (s *daemonVaultCustody) RemovePasswordVault(issuer, account string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if issuer != s.issuer || account != s.account {
		return config.ErrCredentialStoreUnavailable
	}
	_, err := s.request(s.command.Context(), "remove", nil)
	return err
}
