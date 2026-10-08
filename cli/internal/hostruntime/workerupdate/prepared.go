package workerupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/binarytarget"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
)

var (
	ErrApprovalRequired  = errors.New("exact prepared update requires approval")
	ErrPreparedCandidate = errors.New("prepared update is unavailable or invalid")
)

// PreparedCandidate identifies the signed artifact, never its private staging path
// or the executable extracted from a signed macOS package.
type PreparedCandidate struct {
	ID               string `json:"id"`
	Version          string `json:"version"`
	Platform         string `json:"platform"`
	Architecture     string `json:"architecture"`
	SHA256           string `json:"sha256"`
	Length           int64  `json:"length"`
	OwnerMaintenance bool   `json:"owner_maintenance"`
}

// PreparedCandidateForRelease binds approval to all verified release metadata,
// including its activation policy and companion artifacts.
func PreparedCandidateForRelease(release Release) (PreparedCandidate, error) {
	if validateRelease(release) != nil || release.LocalSource != nil {
		return PreparedCandidate{}, ErrInvalidRelease
	}
	raw, err := json.Marshal(release)
	if err != nil {
		return PreparedCandidate{}, ErrInvalidRelease
	}
	digest := sha256.Sum256(raw)
	return PreparedCandidate{ID: hex.EncodeToString(digest[:]), Version: release.Version, Platform: release.Platform, Architecture: release.Architecture, SHA256: release.SHA256, Length: release.Length, OwnerMaintenance: release.SupervisorMaintenance}, nil
}

func candidateFromJournal(j updateflow.Journal) PreparedCandidate {
	return PreparedCandidate{ID: j.CandidateID, Version: j.CandidateVersion, Platform: j.ArtifactPlatform, Architecture: j.ArtifactArchitecture, SHA256: j.ArtifactDigest, Length: j.ArtifactLength, OwnerMaintenance: j.SupervisorMaintenance}
}

// Prepare downloads and verifies an artifact but never launches it or disturbs
// the running installation. Restart retains this candidate without installing it.
func (m *Manager) Prepare(ctx context.Context, release Release) (PreparedCandidate, error) {
	if err := ctx.Err(); err != nil {
		return PreparedCandidate{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return PreparedCandidate{}, err
	}
	candidate, err := PreparedCandidateForRelease(release)
	if err != nil {
		return PreparedCandidate{}, err
	}
	prior, loadErr := updateflow.Load(m.config.StatePath)
	if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		return PreparedCandidate{}, errors.Join(ErrPreparedCandidate, loadErr)
	}
	if loadErr == nil && prior.Stage != updateflow.StageIdle && prior.Stage != updateflow.StageChecking && prior.Stage != updateflow.StageAwaitingApproval {
		return PreparedCandidate{}, ErrBlocked
	}
	if err = m.recoverLocked(ctx); err != nil {
		return PreparedCandidate{}, err
	}
	if (m.active.LocalSource == nil || m.active.LocalSource.Distribution == installsource.Official) && compareVersion(release.Version, m.active.Version) <= 0 {
		if release.Version == m.active.Version {
			return PreparedCandidate{}, nil
		}
		return PreparedCandidate{}, ErrInvalidRelease
	}
	if quarantine, e := m.quarantinedLocked(); e != nil {
		return PreparedCandidate{}, e
	} else if quarantine == release.Version {
		return PreparedCandidate{}, ErrQuarantined
	}
	previous, err := updateflow.Load(m.config.StatePath)
	if err != nil {
		return PreparedCandidate{}, err
	}
	if previous.Stage == updateflow.StageAwaitingApproval && previous.CandidateID == candidate.ID {
		if err = m.verifyPrepared(ctx, previous); err != nil {
			return PreparedCandidate{}, err
		}
		return candidate, nil
	}
	journal := m.newJournal()
	m.record(ctx, EventScheduled, journal, release, "")
	if err = m.write(journal); err != nil {
		return PreparedCandidate{}, err
	}
	journal, err = m.transition(journal, updateflow.StageChecking)
	if err != nil {
		return PreparedCandidate{}, err
	}
	if err = m.write(journal); err != nil {
		return PreparedCandidate{}, err
	}
	local, err := m.stage(ctx, release)
	if err != nil {
		return PreparedCandidate{}, errors.Join(err, m.removeStaged(), m.write(m.idleJournal(journal, updateflow.FailureVerification, "")))
	}
	m.record(ctx, EventDownloading, journal, release, "")
	journal = withRelease(journal, local, m.config.BinaryStaged)
	journal.CandidateID = candidate.ID
	journal.ArtifactDigest, journal.ArtifactLength = candidate.SHA256, candidate.Length
	journal.ArtifactPlatform, journal.ArtifactArchitecture = candidate.Platform, candidate.Architecture
	journal, err = m.transition(journal, updateflow.StageAwaitingApproval)
	if err != nil {
		return PreparedCandidate{}, err
	}
	if err = m.write(journal); err != nil {
		return PreparedCandidate{}, err
	}
	return candidate, nil
}

func (m *Manager) PreparedCandidate() (PreparedCandidate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	journal, err := updateflow.Load(m.config.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return PreparedCandidate{}, nil
	}
	if err != nil {
		return PreparedCandidate{}, errors.Join(ErrPreparedCandidate, err)
	}
	if journal.Stage != updateflow.StageAwaitingApproval {
		return PreparedCandidate{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.config.RollbackTimeout)
	defer cancel()
	if err = m.verifyPrepared(ctx, journal); err != nil {
		return PreparedCandidate{}, err
	}
	return candidateFromJournal(journal), nil
}

// Approve records exact user intent only. It never starts an installation.
func (m *Manager) Approve(ctx context.Context, candidateID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	journal, err := updateflow.Load(m.config.StatePath)
	if err != nil {
		return errors.Join(ErrPreparedCandidate, err)
	}
	if candidateID == "" || journal.Stage != updateflow.StageAwaitingApproval || journal.CandidateID != candidateID {
		return ErrApprovalRequired
	}
	if err = m.verifyPrepared(ctx, journal); err != nil {
		return err
	}
	journal.ApprovedCandidateID = candidateID
	return m.write(journal)
}

func (m *Manager) verifyPrepared(ctx context.Context, j updateflow.Journal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if j.StagedPath != m.config.BinaryStaged || j.CandidateID == "" || j.ArtifactPlatform != m.active.Platform || j.ArtifactArchitecture != m.active.Architecture {
		return ErrPreparedCandidate
	}
	if err := safeRuntimeFile(j.StagedPath, m.config.OwnerUID, true); err != nil {
		return errors.Join(ErrPreparedCandidate, err)
	}
	if !regularMatches(j.StagedPath, j.CandidateLength, j.CandidateDigest) {
		return ErrPreparedCandidate
	}
	if err := binarytarget.Validate(j.StagedPath, j.ArtifactPlatform, j.ArtifactArchitecture); err != nil {
		return errors.Join(ErrPreparedCandidate, err)
	}
	if err := m.config.NativeVerifier.Verify(ctx, j.StagedPath, j.ArtifactPlatform, j.ArtifactArchitecture); err != nil {
		return errors.Join(ErrPreparedCandidate, err)
	}
	return nil
}
