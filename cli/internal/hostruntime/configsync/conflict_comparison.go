package configsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

var ErrConflictComparisonStale = errors.New("config conflict changed; refresh its status before comparing")

type ConflictComparisonRequest struct {
	AssignmentID           string `json:"assignment_id"`
	AssignmentVersion      int64  `json:"assignment_version"`
	Path                   string `json:"path"`
	ConflictRevision       string `json:"conflict_revision"`
	ExpectedRemoteRevision string `json:"expected_remote_revision"`
}
type ConflictComparisonSide struct {
	Present bool   `json:"present"`
	Kind    string `json:"kind"`
	SHA256  string `json:"sha256"`
	Content []byte `json:"content"`
}
type ConflictComparison struct {
	ConflictComparisonRequest
	Local   ConflictComparisonSide `json:"local"`
	Managed ConflictComparisonSide `json:"managed"`
}

// CompareConflict fetches the current branch before inspecting the conflict.
// Repository and reconciler locks keep publication and comparison serialized.
func (r *GitRepository) CompareConflict(ctx context.Context, request ConflictComparisonRequest) (ConflictComparison, error) {
	remote, err := r.Fetch(ctx)
	if err != nil {
		return ConflictComparison{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if remote.Revision != request.ExpectedRemoteRevision || r.lastRemote != remote.Revision {
		return ConflictComparison{}, ErrConflictComparisonStale
	}
	reader, ok := r.reconciler.(interface {
		CompareConflict(context.Context, string, string, ConflictComparisonRequest) (ConflictComparison, error)
	})
	if !ok {
		return ConflictComparison{}, ErrWorkspaceReconcilerInvalid
	}
	return reader.CompareConflict(ctx, r.root, remote.Revision, request)
}

// CompareConflict returns current content only while all reported conflict
// identities still agree. It never applies a resolution or changes a worktree.
func (r *PlaintextWorkspaceReconciler) CompareConflict(ctx context.Context, repositoryRoot, remoteRevision string, request ConflictComparisonRequest) (ConflictComparison, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := ConflictComparison{ConflictComparisonRequest: request}
	var statusBefore []byte
	if r.mapping == nil {
		status, raw, err := r.loadComparisonState(ctx, repositoryRoot, request)
		if err != nil {
			return result, err
		}
		statusBefore = raw
		defer func() { r.mapping = nil; r.diagnostics = ReconciliationDiagnostics{} }()
		r.diagnostics.Conflicts = append([]PathSummary(nil), status.Conflicts...)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if request.AssignmentID != r.descriptor.AssignmentID || request.AssignmentVersion != r.descriptor.AssignmentVersion || request.ExpectedRemoteRevision != remoteRevision || !plumbing.IsHash(remoteRevision) || !safeConflictRevision(request.ConflictRevision) || !safeRelativeStatusPath(request.Path) || r.mapping == nil {
		return result, ErrConflictComparisonStale
	}
	found := false
	for _, conflict := range r.diagnostics.Conflicts {
		if conflict.Path == request.Path && conflict.Revision == request.ConflictRevision {
			found = true
		}
	}
	if !found {
		return result, ErrConflictComparisonStale
	}
	target, ok := r.mapping.LocalPath(request.Path)
	if !ok || !r.mapping.eligible(request.Path, target, r.descriptor.Policy, r.manifest) {
		return result, ErrConflictComparisonStale
	}
	metadataPath := filepath.Join(r.stateRoot, "conflicts", request.ConflictRevision, "metadata.json")
	info, err := os.Lstat(metadataPath)
	if err != nil || !privateControlFile(metadataPath, info) || checkSafeAbsolutePath(metadataPath) != nil {
		return result, ErrConflictComparisonStale
	}
	raw, opened, err := secureReadFile(metadataPath, 32<<10)
	if err != nil || !os.SameFile(info, opened) {
		return result, ErrConflictComparisonStale
	}
	var metadata struct {
		RepositoryID   string      `json:"repository_id"`
		AssignmentID   string      `json:"assignment_id"`
		BaseRevision   string      `json:"base_revision"`
		RemoteRevision string      `json:"remote_revision"`
		BaseDigest     string      `json:"base_digest"`
		LocalDigest    string      `json:"local_digest"`
		RemoteDigest   string      `json:"remote_digest"`
		Conflict       PathSummary `json:"conflict"`
	}
	if json.Unmarshal(raw, &metadata) != nil || metadata.RepositoryID != r.descriptor.RepositoryID || metadata.AssignmentID != request.AssignmentID || metadata.Conflict.Path != request.Path || metadata.Conflict.Revision != request.ConflictRevision {
		return result, ErrConflictComparisonStale
	}
	result.Local, err = readComparisonLocal(target, r.descriptor.Policy.MaxFileBytes)
	if err != nil {
		return result, err
	}
	if comparisonIdentity(result.Local) != metadata.LocalDigest {
		return ConflictComparison{}, ErrConflictComparisonStale
	}
	repository, err := git.PlainOpen(repositoryRoot)
	if err != nil {
		return ConflictComparison{}, err
	}
	commit, err := repository.CommitObject(plumbing.NewHash(remoteRevision))
	if err != nil {
		return ConflictComparison{}, err
	}
	tree, err := commit.Tree()
	if err != nil {
		return ConflictComparison{}, err
	}
	file, err := tree.File(request.Path)
	if errors.Is(err, object.ErrFileNotFound) {
		result.Managed = ConflictComparisonSide{Kind: "deleted"}
	} else if err != nil {
		return ConflictComparison{}, err
	} else {
		if file.Size < 0 || file.Size > r.descriptor.Policy.MaxFileBytes || file.Size+int64(len(result.Local.Content)) > r.descriptor.Policy.MaxBatchBytes {
			return ConflictComparison{}, ErrSourceChanged
		}
		reader, err := file.Reader()
		if err != nil {
			return ConflictComparison{}, err
		}
		content, readErr := io.ReadAll(io.LimitReader(reader, r.descriptor.Policy.MaxFileBytes+1))
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || int64(len(content)) != file.Size {
			return ConflictComparison{}, errors.Join(ErrSourceChanged, readErr, closeErr)
		}
		kind := "file"
		hash := hashSnapshotBytes(content)
		if file.Mode == filemode.Symlink {
			kind = "symlink"
			hash = hashSnapshotBytes(append([]byte("symlink:"), content...))
		} else if file.Mode != filemode.Regular && file.Mode != filemode.Executable && file.Mode != filemode.Deprecated {
			return ConflictComparison{}, ErrSourceChanged
		}
		result.Managed = ConflictComparisonSide{Present: true, Kind: kind, SHA256: hash, Content: content}
	}
	if comparisonIdentity(result.Managed) != metadata.RemoteDigest {
		return ConflictComparison{}, ErrConflictComparisonStale
	}
	if err := ctx.Err(); err != nil {
		return ConflictComparison{}, err
	}
	if statusBefore != nil {
		_, raw, err := r.readComparisonStatus(request)
		if err != nil || !bytes.Equal(statusBefore, raw) {
			return ConflictComparison{}, ErrConflictComparisonStale
		}
		// The worker runs in another process; never cache its mutable state.
		r.mapping = nil
		r.diagnostics = ReconciliationDiagnostics{}
	}
	return result, nil
}
func (r *PlaintextWorkspaceReconciler) readComparisonStatus(request ConflictComparisonRequest) (Status, []byte, error) {
	path := r.comparisonStatusPath
	if checkSafeAbsolutePath(path) != nil {
		return Status{}, nil, ErrConflictComparisonStale
	}
	info, err := os.Lstat(path)
	if err != nil || !privateControlFile(path, info) {
		return Status{}, nil, ErrConflictComparisonStale
	}
	raw, opened, err := secureReadFile(path, maxControlResponseBytes)
	if err != nil || !os.SameFile(info, opened) {
		return Status{}, nil, ErrConflictComparisonStale
	}
	var status Status
	if json.Unmarshal(raw, &status) != nil || status.Validate(r.descriptor.Policy.SummaryLimit) != nil || status.RepositoryID != r.descriptor.RepositoryID || status.AssignmentID != request.AssignmentID || status.EnvironmentID != r.descriptor.EnvironmentID || status.MachineID != r.descriptor.MachineID || status.InstallationGeneration != r.descriptor.InstallationGeneration || status.RemoteRevision != request.ExpectedRemoteRevision || (status.State != "conflict" && status.State != "warning") {
		return Status{}, nil, ErrConflictComparisonStale
	}
	return status, raw, nil
}
func (r *PlaintextWorkspaceReconciler) loadComparisonState(ctx context.Context, repositoryRoot string, request ConflictComparisonRequest) (Status, []byte, error) {
	status, raw, err := r.readComparisonStatus(request)
	if err != nil {
		return status, nil, err
	}
	var shared SourceConfig
	if r.sharedSource != nil {
		shared, err = r.sharedSource(ctx)
	} else {
		shared, err = LoadSharedSourceConfig(repositoryRoot, DefaultSourceConfigLimits())
	}
	if err != nil {
		return status, nil, err
	}
	effective, err := MergeSourceConfigs(shared, r.machineSource)
	if err != nil {
		return status, nil, err
	}
	approved := r.approvedConfiguration
	if !effective.Enabled || effective.Revision != approved.ConfigurationRevision || ProjectionRevision(effective.Mode, effective.AutomaticUpdates, effective.PathRules) != ProjectionRevision(approved.Mode, approved.AutomaticUpdates, approved.PathRules) || !r.targetsMatch(effective) {
		return status, nil, ErrConfigurationChanged
	}
	mapping, err := ResolveEffectiveConfig(r.homeRoot, effective)
	if err != nil {
		return status, nil, err
	}
	manifest := effective.Manifest()
	if status.ManifestRevision != manifest.Revision {
		return status, nil, ErrConflictComparisonStale
	}
	r.mapping = mapping
	r.manifest = manifest
	return status, raw, nil
}
func comparisonIdentity(side ConflictComparisonSide) string {
	if !side.Present {
		return "deleted"
	}
	return side.SHA256
}
func readComparisonLocal(target string, maxBytes int64) (ConflictComparisonSide, error) {
	empty := ConflictComparisonSide{Kind: "deleted"}
	if checkSafeAbsolutePath(filepath.Dir(target)) != nil {
		return empty, ErrSourceChanged
	}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(target)
		if err != nil {
			return empty, err
		}
		after, err := os.Lstat(target)
		if err != nil || !os.SameFile(info, after) || info.ModTime() != after.ModTime() {
			return empty, ErrSourceChanged
		}
		content := []byte(filepath.ToSlash(link))
		if int64(len(content)) > maxBytes {
			return empty, ErrSourceChanged
		}
		return ConflictComparisonSide{Present: true, Kind: "symlink", SHA256: hashSnapshotBytes(append([]byte("symlink:"), content...)), Content: content}, nil
	}
	if !info.Mode().IsRegular() || !safeSnapshotPermissions(target, info) || info.Size() > maxBytes {
		return empty, ErrSourceChanged
	}
	content, opened, err := secureReadFile(target, maxBytes)
	if err != nil || !os.SameFile(info, opened) {
		return empty, errors.Join(ErrSourceChanged, err)
	}
	after, err := os.Lstat(target)
	if err != nil || !os.SameFile(opened, after) || after.Size() != int64(len(content)) || opened.ModTime() != after.ModTime() {
		return empty, ErrSourceChanged
	}
	return ConflictComparisonSide{Present: true, Kind: "file", SHA256: hashSnapshotBytes(content), Content: content}, nil
}
