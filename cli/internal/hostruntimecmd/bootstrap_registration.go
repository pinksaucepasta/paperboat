package hostruntimecmd

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/inbox"
)

// saveBootstrapRegistration establishes the local machine identity before a
// service is installed. A fresh `pb pair` must therefore be restartable and
// must not depend on a prior interactive `pb setup` call.
func saveBootstrapRegistration(store *identity.Store, serverURL string, material bootstrap.Material, sshUser string, sshPort uint16) error {
	inboxPath, err := inbox.DefaultPath()
	if err != nil {
		return err
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
		return errors.New("Paperboat enrollment account is unavailable")
	}
	sshUser, sshPort = bootstrapSSHFields(material.SetupMode, sshUser, sshPort)
	return store.SaveRegistration(identity.Registration{
		ServerURL:              strings.TrimRight(strings.TrimSpace(serverURL), "/"),
		AccountID:              profile.Account.ID,
		MachineID:              material.UserMachineID,
		EnvironmentID:          material.EnvironmentID,
		PublicKeyID:            key.ID,
		PublicIdentityKey:      base64.RawURLEncoding.EncodeToString(key.Public()),
		InboxPath:              inboxPath,
		InstallationGeneration: material.InstallationGeneration,
		SetupMode:              material.SetupMode,
		SetupRoles:             append([]string(nil), material.SetupRoles...),
		SSHUser:                strings.TrimSpace(sshUser),
		SSHPort:                sshPort,
		UpdatedAt:              time.Now().UTC(),
	})
}

func bootstrapSSHFields(setupMode, sshUser string, sshPort uint16) (string, uint16) {
	if setupMode != "host" {
		return "", 0
	}
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

func validateBootstrapFinalizationBinding(registration identity.Registration, accountID string, resume bootstrap.ResumeRecord) error {
	material := resume.Material
	if !resume.RuntimeReady || !resume.RuntimeEnrolled || !resume.ClientInstalled || material == nil ||
		accountID == "" || registration.AccountID != accountID || registration.ServerURL != resume.ServerURL ||
		registration.PublicIdentityKey != resume.PublicIdentityKey || registration.MachineID != material.UserMachineID ||
		registration.EnvironmentID != material.EnvironmentID || registration.InstallationGeneration != material.InstallationGeneration ||
		registration.SetupMode != material.SetupMode {
		return fmt.Errorf("existing enrollment finalization: %w", bootstrap.ErrResumeBinding)
	}
	return nil
}

func unixBootstrapSSHFields(setupMode, username string) (string, uint16) {
	if setupMode != "host" {
		return "", 0
	}
	return bootstrapSSHFields(setupMode, username, 22)
}
