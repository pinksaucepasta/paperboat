package configsync

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	git "github.com/go-git/go-git/v5"
)

func (r *PlaintextWorkspaceReconciler) preserveConflicts(
	baseline, local, remote map[string]FileState,
	conflicts []PathSummary,
	baseHome, remoteHome, baseRevision, remoteRevision string,
) error {
	for _, conflict := range conflicts {
		conflictRoot := filepath.Join(r.stateRoot, "conflicts", conflict.Revision)
		if err := os.MkdirAll(conflictRoot, 0o700); err != nil {
			return err
		}
		for _, side := range []struct {
			name  string
			root  string
			state FileState
			ok    bool
		}{
			{"base", baseHome, baseline[conflict.Path], hasState(baseline, conflict.Path)},
			{"local", r.homeRoot, local[conflict.Path], hasState(local, conflict.Path)},
			{"remote", remoteHome, remote[conflict.Path], hasState(remote, conflict.Path)},
		} {
			target := filepath.Join(conflictRoot, filepath.FromSlash(conflict.Path)+"."+side.name)
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			if info, statErr := os.Lstat(target); statErr == nil {
				if !privateControlFile(target, info) {
					return ErrConfigRepositoryInvalid
				}
				continue
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return statErr
			}
			if !side.ok {
				if err := writePrivateAtomic(target, []byte(`{"deleted":true}`+"\n")); err != nil {
					return err
				}
				continue
			}
			if side.root == "" {
				return ErrBaselineInvalid
			}
			if side.state.Mode&os.ModeSymlink != 0 {
				if err := writePrivateAtomic(target, []byte(side.state.Target)); err != nil {
					return err
				}
				continue
			}
			source := filepath.Join(side.root, filepath.FromSlash(conflict.Path))
			if side.name == "local" {
				var ok bool
				source, ok = r.mapping.LocalPath(conflict.Path)
				if !ok {
					return ErrPathRuleInvalid
				}
			}
			if err := writeConflictFile(target, source, side.state); err != nil {
				return err
			}
		}
		metadata, err := json.Marshal(struct {
			RepositoryID   string      `json:"repository_id"`
			AssignmentID   string      `json:"assignment_id"`
			BaseRevision   string      `json:"base_revision,omitempty"`
			RemoteRevision string      `json:"remote_revision"`
			BaseDigest     string      `json:"base_digest"`
			LocalDigest    string      `json:"local_digest"`
			RemoteDigest   string      `json:"remote_digest"`
			Conflict       PathSummary `json:"conflict"`
		}{
			r.descriptor.RepositoryID, r.descriptor.AssignmentID, baseRevision, remoteRevision,
			stateIdentity(baseline, conflict.Path), stateIdentity(local, conflict.Path),
			stateIdentity(remote, conflict.Path), conflict,
		})
		if err != nil {
			return err
		}
		if err := writePrivateAtomic(filepath.Join(conflictRoot, "metadata.json"), append(metadata, '\n')); err != nil {
			return err
		}
	}
	return nil
}

func hasState(states map[string]FileState, path string) bool {
	_, ok := states[path]
	return ok
}

func writeConflictFile(target, source string, expected FileState) error {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Size() != expected.Bytes {
		return errors.Join(ErrSourceChanged, err)
	}
	value, opened, err := secureReadFile(source, expected.Bytes)
	if err == nil && (!os.SameFile(info, opened) || hashSnapshotBytes(value) != expected.Hash) {
		return ErrSourceChanged
	}
	if err != nil {
		return err
	}
	return writePrivateAtomic(target, value)
}

func removeManagedPath(root, relative string) error {
	if !safeRelativeStatusPath(relative) {
		return ErrWorkspaceReconcilerInvalid
	}
	target := filepath.Join(root, filepath.FromSlash(relative))
	if !sameOrInsidePath(target, root) {
		return ErrWorkspaceReconcilerInvalid
	}
	return secureRemoveFile(target)
}

func validateRepositoryBatch(root string, status git.Status, policy RuntimePolicy) error {
	var total int64
	for relative, fileStatus := range status {
		if relative == ".git" || strings.HasPrefix(relative, ".git/") ||
			fileStatus.Worktree == git.Deleted || fileStatus.Staging == git.Deleted {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(relative))
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.Join(ErrConfigRepositoryInvalid, err)
		}
		if info.Size() > policy.MaxFileBytes+(64<<10) {
			return ErrConfigRepositoryInvalid
		}
		total += info.Size()
		if total > policy.MaxBatchBytes+(1<<20) {
			return ErrConfigRepositoryInvalid
		}
	}
	return nil
}

func boundPathSummaries(values []PathSummary, limit int) []PathSummary {
	sort.Slice(values, func(i, j int) bool {
		if values[i].Path == values[j].Path {
			return values[i].Reason < values[j].Reason
		}
		return values[i].Path < values[j].Path
	})
	result := values[:0]
	for _, value := range values {
		if len(result) > 0 && result[len(result)-1].Path == value.Path && result[len(result)-1].Reason == value.Reason {
			continue
		}
		result = append(result, value)
		if len(result) == limit {
			break
		}
	}
	return result
}

func (r *PlaintextWorkspaceReconciler) observeLocal(files map[string]FileState, deleted []string) {
	r.observed = cloneFileStates(files)
	for _, name := range deleted {
		r.observed[name] = FileState{}
	}
}

// ObservedLocalFile lets watches distinguish already-reconciled output from
// subsequent user changes without ignoring edits that happen during sync.
func (r *PlaintextWorkspaceReconciler) ObservedLocalFile(name string) (FileState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state, ok := r.observed[name]
	return state, ok
}

func (r *PlaintextWorkspaceReconciler) targetsMatch(config EffectiveConfig) bool {
	for direction, entry := range []struct {
		target *RepositoryTarget
		id     string
	}{{config.Pull, r.approvedConfiguration.PullRepositoryID}, {config.Push, r.approvedConfiguration.PushRepositoryID}} {
		if direction == 0 && config.Mode == ModePushOnly || direction == 1 && config.Mode == ModePullOnly {
			continue
		}
		if entry.target == nil {
			continue
		}
		id := entry.id
		if id == "" {
			id = r.approvedConfiguration.RepositoryID
		}
		if entry.target.RepositoryID != id {
			return false
		}
		if entry.target.URL != "" {
			matched := false
			for _, binding := range r.approvedConfiguration.RepositoryBindings {
				if binding.RepositoryID == id && entry.target.URL == binding.URL {
					matched = true
				}
			}
			if !matched {
				return false
			}
		}
	}
	return true
}
