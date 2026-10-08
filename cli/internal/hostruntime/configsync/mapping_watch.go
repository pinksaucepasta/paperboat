package configsync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
)

func (m *PathMapping) managedEvent(full string, policy RuntimePolicy, manifest Manifest) bool {
	for _, r := range m.rules {
		if protectedAbsoluteDestination(full, policy) {
			return false
		}
		if r.Kind == "file" && full == r.LocalPath {
			return m.eligible(r.RepositoryPath, full, policy, manifest)
		}
		if r.Kind != "directory" || !sameOrInsidePath(full, r.LocalPath) {
			continue
		}
		suffix, err := filepath.Rel(r.LocalPath, full)
		if err != nil {
			return false
		}
		if suffix == "." {
			return true
		}
		name := r.RepositoryPath + "/" + filepath.ToSlash(suffix)
		if _, ok := m.LocalPath(name); ok && m.eligible(name, full, policy, manifest) {
			return true
		}
		// Directory creation/deletion may change selected descendants. Individual
		// file events still require exact selection and eligibility.
		if info, err := os.Lstat(full); err == nil && info.IsDir() && !ruleExcludesDirectory(r, filepath.ToSlash(suffix)) && !mandatoryExcluded(name, policy) {
			return true
		}
	}
	return false
}
func resetMappedWatches(watcher *fsnotify.Watcher, m *PathMapping, policy RuntimePolicy, manifest Manifest) error {
	for _, watched := range watcher.WatchList() {
		if err := watcher.Remove(watched); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	seen := map[string]bool{}
	add := func(dir string) error {
		if seen[dir] {
			return nil
		}
		if err := checkSafeAbsolutePath(dir); err != nil {
			return err
		}
		if err := watcher.Add(dir); err != nil {
			return err
		}
		seen[dir] = true
		return nil
	}
	for _, r := range m.rules {
		if protectedAbsoluteDestination(r.LocalPath, policy) {
			continue
		}
		start := r.LocalPath
		if r.Kind == "file" {
			start = filepath.Dir(start)
		}
		for {
			info, err := os.Lstat(start)
			if err == nil && info.IsDir() {
				break
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			parent := filepath.Dir(start)
			if parent == start {
				return ErrPathRuleInvalid
			}
			start = parent
		}
		if err := add(start); err != nil {
			return err
		}
		if r.Kind != "directory" || start != r.LocalPath {
			continue
		}
		err := filepath.WalkDir(start, func(full string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				return nil
			}
			suffix, _ := filepath.Rel(start, full)
			name := r.RepositoryPath + "/" + filepath.ToSlash(suffix)
			physical := strings.TrimPrefix(filepath.ToSlash(full), "/")
			if protectedAbsoluteDestination(full, policy) {
				return filepath.SkipDir
			}
			if suffix != "." && (ruleExcludesDirectory(r, filepath.ToSlash(suffix)) || mandatoryExcluded(name, policy) || mandatoryExcluded(physical, policy) || manifestHardExcluded(physical)) {
				return filepath.SkipDir
			}
			return add(full)
		})
		if err != nil {
			return err
		}
	}
	return nil
}
