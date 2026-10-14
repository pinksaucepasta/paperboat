//go:build windows

package hostruntimecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

type suppliedWindowsInstallation struct {
	ownerSID string
	offline  bool
	identity *hostinstall.WindowsRollbackIdentity
	journal  []byte
}
type suppliedWindowsSnapshot struct {
	Installation  []byte                              `json:"installation"`
	Identity      hostinstall.WindowsRollbackIdentity `json:"identity"`
	Journal       []byte                              `json:"updater_journal,omitempty"`
	RepositoryURL string                              `json:"repository_url"`
	Offline       bool                                `json:"offline"`
}

func (s suppliedWindowsInstallation) Capture(ctx context.Context) ([]byte, error) {
	if s.identity == nil {
		return nil, hostinstall.ErrInvalidRequest
	}
	identity, journal, err := updated.InspectWindowsNativeInstallation(ctx, s.ownerSID, s.offline)
	if err != nil {
		return nil, err
	}
	if identity == nil || *identity != *s.identity || !bytes.Equal(journal, s.journal) {
		return nil, updated.ErrWindowsActivationUnavailable
	}
	state, err := hostinstall.WindowsInstallationStateForRecovery(s.ownerSID, &s.identity.Source, s.offline)
	if err != nil {
		return nil, err
	}
	installation, err := state.Capture(ctx)
	if err != nil {
		return nil, err
	}
	instance, err := hostinstall.WindowsInstanceForSID(s.ownerSID)
	if err != nil {
		return nil, err
	}
	config, err := hostinstall.LoadWindowsRuntimeConfigForInstance(instance)
	if err != nil {
		return nil, err
	}
	return json.Marshal(suppliedWindowsSnapshot{Installation: installation, Identity: *s.identity, Journal: s.journal, RepositoryURL: config.Artifact.RepositoryURL, Offline: s.offline})
}

// Both restored files and committed-journal retirement stay behind the
// lifecycle's in-lock recovery authorization. Raw signed snapshots remain owned
// exclusively by their original prepared helper; this wrapper never unwraps them.
func (s suppliedWindowsInstallation) AuthorizeRecovery(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	if len(data) > 128<<10 {
		return hostinstall.ErrInvalidRequest
	}
	var marker struct {
		CandidateID string `json:"prepared_candidate_id"`
	}
	if json.Unmarshal(data, &marker) != nil {
		return hostinstall.ErrInvalidRequest
	}
	if marker.CandidateID != "" {
		state, err := hostinstall.WindowsInstallationState(s.ownerSID, nil)
		if err != nil {
			return err
		}
		return state.(service.NativeInstallationRecoveryAuthorizer).AuthorizeRecovery(ctx, data)
	}
	var snapshot suppliedWindowsSnapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&snapshot) != nil || decoder.Decode(&struct{}{}) != io.EOF || snapshot.Identity.Source.Validate() != nil {
		return hostinstall.ErrInvalidRequest
	}
	state, err := hostinstall.WindowsInstallationStateForRecovery(s.ownerSID, &snapshot.Identity.Source, snapshot.Offline)
	if err != nil {
		return err
	}
	return state.(service.NativeInstallationRecoveryAuthorizer).AuthorizeRecovery(ctx, snapshot.Installation)
}

func (s suppliedWindowsInstallation) Restore(ctx context.Context, data []byte) error {
	if err := s.AuthorizeRecovery(ctx, data); err != nil {
		return err
	}
	var snapshot suppliedWindowsSnapshot
	if len(data) == 0 || len(data) > 128<<10 {
		return hostinstall.ErrInvalidRequest
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&snapshot) != nil || decoder.Decode(&struct{}{}) != io.EOF || snapshot.Identity.Source.Validate() != nil {
		return hostinstall.ErrInvalidRequest
	}
	if snapshot.Identity.Signed {
		layout, err := service.WindowsUserLayout(s.ownerSID)
		if err != nil {
			return err
		}
		source := workerupdate.TUFSource{RepositoryURL: snapshot.RepositoryURL, StateRoot: filepath.Join(layout.UpdateStateRoot, "tuf")}
		if snapshot.Offline {
			err = source.AuthorizeOfflineRecovery(ctx, snapshot.Identity.Source.Version, "windows", snapshot.Identity.Source.Architecture)
		} else {
			err = source.AuthorizeRecovery(ctx, snapshot.Identity.Source.Version, "windows", snapshot.Identity.Source.Architecture)
		}
		if err != nil {
			return err
		}
	}
	state, err := hostinstall.WindowsInstallationStateForRecovery(s.ownerSID, &snapshot.Identity.Source, snapshot.Offline)
	if err != nil {
		return err
	}
	if err := state.Restore(ctx, snapshot.Installation); err != nil {
		return err
	}
	return updated.RestoreWindowsNativeInstallationJournal(ctx, s.ownerSID, snapshot.Identity, snapshot.Journal)
}
func recoverSuppliedWindowsInstallation(ctx context.Context, ownerSID string, offline bool) error {
	instance, err := hostinstall.WindowsInstanceForSID(ownerSID)
	if err != nil {
		return err
	}
	installed, err := hostinstall.LoadWindowsRuntimeConfigForInstance(instance)
	if err != nil {
		return err
	}
	layout, err := service.WindowsUserLayout(ownerSID)
	if err != nil {
		return err
	}
	// The approved signed helper owns its native receipt and deployment gates.
	// Do not hold the publication mutex while starting or waiting for that owner.
	config := windowsUpdatedConfigFor(installed, layout, installed.Source.Version)
	recoverSigned := updated.RecoverWindowsNativeInstall
	if offline {
		recoverSigned = updated.CheckOfflineWindowsNativeInstall
	}
	if err := recoverSigned(ctx, config); err != nil {
		return err
	}
	return hostinstall.RecoverWindowsInstallation(ctx, ownerSID, suppliedWindowsInstallation{ownerSID: ownerSID})
}

// Maintenance remains a pre-stop gate. Recovery of an already persisted native
// transaction precedes it because a crash can leave the old endpoint stopped.
func prepareSuppliedWindowsSnapshot(ctx context.Context, ownerSID string, offline bool) (suppliedWindowsInstallation, error) {
	identity, journal, err := updated.InspectWindowsNativeInstallation(ctx, ownerSID, offline)
	return suppliedWindowsInstallation{ownerSID: ownerSID, offline: offline, identity: identity, journal: journal}, err
}
