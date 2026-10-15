package hostruntimecmd

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/inbox"
)

// saveBootstrapRegistration establishes the local machine identity before a
// service is installed. A fresh `pb pair` must therefore be restartable and
// must not depend on a prior interactive `pb setup` call.
func saveBootstrapRegistration(store *identity.Store, serverURL string, material bootstrap.Material, sshUser string, sshPort uint16) error {
	previous, err := store.Registration()
	var inboxPath string
	if err == nil {
		inboxPath = previous.InboxPath
	} else if runtimeCausesOnly(err, os.ErrNotExist) {
		inboxPath, err = inbox.DefaultPath()
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("read existing Inbox registration: %w", err)
	}
	if err := inbox.EnsurePath(inboxPath); err != nil {
		return err
	}
	key := store.Current()
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	cfg.ServerURL = strings.TrimRight(strings.TrimSpace(serverURL), "/")
	profiles, err := config.ProfileStoreFor(cfg)
	if err != nil {
		return err
	}
	profile, err := profiles.Load(cfg.ServerURL)
	if err != nil || strings.TrimSpace(profile.Account.ID) == "" {
		return runtimeFailure("Paperboat enrollment account is unavailable", err)
	}
	sshUser, sshPort = bootstrapSSHFields(sshUser, sshPort)
	return store.SaveRegistration(identity.Registration{
		ServerURL:              strings.TrimRight(strings.TrimSpace(serverURL), "/"),
		AccountID:              profile.Account.ID,
		MachineID:              material.UserMachineID,
		EnvironmentID:          material.EnvironmentID,
		PublicKeyID:            key.ID,
		PublicIdentityKey:      base64.RawURLEncoding.EncodeToString(key.Public()),
		InboxPath:              inboxPath,
		InstallationGeneration: material.InstallationGeneration,
		SSHUser:                strings.TrimSpace(sshUser),
		SSHPort:                sshPort,
		UpdatedAt:              time.Now().UTC(),
	})
}

func bootstrapSSHFields(sshUser string, sshPort uint16) (string, uint16) {
	return strings.TrimSpace(sshUser), sshPort
}

func validateBootstrapFinalization(store *identity.Store, resume bootstrap.ResumeRecord) error {
	registration, err := store.Registration()
	if err != nil {
		return fmt.Errorf("read existing enrollment for finalization: %w", err)
	}
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	profiles, err := config.ProfileStoreFor(cfg)
	if err != nil {
		return err
	}
	profile, err := profiles.Load(resume.ServerURL)
	if err != nil {
		return fmt.Errorf("load enrolled account for finalization: %w", err)
	}
	return validateBootstrapFinalizationBinding(registration, profile.Account.ID, resume)
}

// validateBootstrapRecovery uses the current profile and its normal refresh
// owner. A protected journal is an installation binding, never login authority.
func validateBootstrapRecovery(ctx context.Context, store *identity.Store, resume bootstrap.ResumeRecord) error {
	if !resume.RuntimeEnrolled || !resume.ClientInstalled || resume.Material == nil {
		return bootstrap.ErrResumeBinding
	}
	registration, err := store.Registration()
	if err != nil {
		return fmt.Errorf("read existing enrollment for recovery: %w", err)
	}
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	profiles, err := config.ProfileStoreFor(cfg)
	if err != nil {
		return err
	}
	profile, err := profiles.Load(resume.ServerURL)
	if err != nil {
		return runtimeFailure("Sign in to the enrolled Paperboat account and run `pb setup` to recover this machine.", err)
	}
	if err := validateBootstrapEnrollmentBinding(registration, profile.Account.ID, resume); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client := api.New(resume.ServerURL, config.Credential{}, nil)
	client.SetCredentialSource((&auth.Source{Store: profiles, Issuer: resume.ServerURL}).WithContext(ctx))
	me, err := client.Me(ctx)
	if err != nil {
		return runtimeFailure("Verify this machine's Paperboat sign-in, then run `pb setup` to recover it.", err)
	}
	if me.ID != profile.Account.ID || me.Status != "active" {
		return runtimeFailure("Sign in to the enrolled Paperboat account and run `pb setup` to recover this machine.", bootstrap.ErrResumeBinding)
	}
	return nil
}

func validateBootstrapFinalizationBinding(registration identity.Registration, accountID string, resume bootstrap.ResumeRecord) error {
	if !resume.RuntimeReady || !resume.RuntimeEnrolled || !resume.ClientInstalled {
		return fmt.Errorf("existing enrollment finalization: %w", bootstrap.ErrResumeBinding)
	}
	return validateBootstrapEnrollmentBinding(registration, accountID, resume)
}

func validateBootstrapEnrollmentBinding(registration identity.Registration, accountID string, resume bootstrap.ResumeRecord) error {
	material := resume.Material
	if material == nil || accountID == "" || registration.AccountID != accountID || registration.ServerURL != resume.ServerURL ||
		registration.PublicIdentityKey != resume.PublicIdentityKey || registration.MachineID != material.UserMachineID ||
		registration.EnvironmentID != material.EnvironmentID || registration.InstallationGeneration != material.InstallationGeneration {
		return fmt.Errorf("existing enrollment profile: %w", bootstrap.ErrResumeBinding)
	}
	return nil
}

func unixBootstrapSSHFields(username string) (string, uint16) {
	return bootstrapSSHFields(username, 22)
}
