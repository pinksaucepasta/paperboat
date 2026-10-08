package configsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

// PathRule records a user's explicit repository-to-machine mapping. It never
// derives a destination from a platform or from a repository filename.
type PathRule struct {
	ID             string   `json:"id"`
	Source         string   `json:"source"`
	RepositoryPath string   `json:"repository_path"`
	LocalPath      string   `json:"local_path"`
	Kind           string   `json:"kind"`
	Include        []string `json:"include"`
	Exclude        []string `json:"exclude"`
}

var ErrPathRuleInvalid = errors.New("invalid config path rule")

type PathRuleError struct{ RuleID, Code string }

func (e *PathRuleError) Error() string {
	return fmt.Sprintf("config path rule %s: %s; choose an explicit safe, writable destination", e.RuleID, e.Code)
}
func (e *PathRuleError) Unwrap() error { return ErrPathRuleInvalid }

type PathMapping struct {
	home     string
	rules    []PathRule
	revision string
}

func ResolvePathRules(home string, rules []PathRule) (*PathMapping, error) {
	return resolvePathRules(home, rules, true)
}
func resolvePathRules(home string, rules []PathRule, inspect bool) (*PathMapping, error) {
	m := &PathMapping{home: home, rules: append([]PathRule(nil), rules...)}
	if len(rules) > 256 {
		return nil, &PathRuleError{Code: "too_many_rules"}
	}
	ids := map[string]bool{}
	for i := range m.rules {
		r := &m.rules[i]
		r.Include = append([]string(nil), r.Include...)
		r.Exclude = append([]string(nil), r.Exclude...)
		fail := func(code string) (*PathMapping, error) { return nil, &PathRuleError{r.ID, code} }
		if len(r.Include)+len(r.Exclude) > 64 || r.ID == "" || ids[r.ID] || !safePortableRepositoryPath(r.RepositoryPath) || (r.Kind != "file" && r.Kind != "directory") {
			return fail("invalid_selection")
		}
		ids[r.ID] = true
		if r.Kind == "file" && len(r.Include)+len(r.Exclude) > 0 {
			return fail("file_patterns_not_allowed")
		}
		for _, pattern := range append(append([]string(nil), r.Include...), r.Exclude...) {
			if len(pattern) > 1024 || pattern == "" || strings.HasPrefix(pattern, "/") || strings.Contains(pattern, "\\") || strings.Contains(pattern, "..") || !doublestar.ValidatePattern(pattern) {
				return fail("invalid_pattern")
			}
		}
		local, err := expandExplicitLocalPath(home, r.LocalPath)
		if err != nil {
			return fail("unsupported_destination")
		}
		r.LocalPath = local
		if inspect {
			if err := checkSafeAbsolutePath(local); err != nil {
				return fail("unsafe_destination")
			}
		}
		for _, other := range m.rules[:i] {
			if (pathsOverlapFold(r.RepositoryPath, other.RepositoryPath) && r.Source == other.Source) || (pathsOverlapFold(filepath.ToSlash(local), filepath.ToSlash(other.LocalPath)) && !correspondingSourceOverlap(*r, other)) {
				return fail("overlapping_selection")
			}
		}
	}
	sort.Slice(m.rules, func(i, j int) bool { return m.rules[i].ID < m.rules[j].ID })
	data, _ := json.Marshal(m.rules)
	digest := sha256.Sum256(data)
	m.revision = hex.EncodeToString(digest[:])
	return m, nil
}

// A higher-priority source may own a nested selection at the exact destination
// the enclosing rule would map it to. Every logical file still has one owner.
func correspondingSourceOverlap(a, b PathRule) bool {
	if a.Source == b.Source {
		return false
	}
	for _, pair := range [][2]PathRule{{a, b}, {b, a}} {
		parent, child := pair[0], pair[1]
		if parent.Kind == "directory" && strings.HasPrefix(child.RepositoryPath, parent.RepositoryPath+"/") {
			suffix := strings.TrimPrefix(child.RepositoryPath, parent.RepositoryPath+"/")
			return filepath.Join(parent.LocalPath, filepath.FromSlash(suffix)) == child.LocalPath
		}
	}
	return false
}
func expandExplicitLocalPath(home, value string) (string, error) {
	for _, part := range strings.Split(strings.ReplaceAll(value, "\\", "/"), "/") {
		if part == ".." || part == "." {
			return "", ErrPathRuleInvalid
		}
	}
	if value == "~" {
		value = home
	} else if strings.HasPrefix(value, "~/") || strings.HasPrefix(value, "~\\") {
		value = filepath.Join(home, value[2:])
	}
	for _, token := range []string{"%APPDATA%", "%LOCALAPPDATA%", "%USERPROFILE%", "$XDG_CONFIG_HOME"} {
		if value == token || strings.HasPrefix(value, token+"/") || strings.HasPrefix(value, token+"\\") {
			key := strings.Trim(token, "%$")
			expanded := os.Getenv(key)
			if !filepath.IsAbs(expanded) {
				return "", ErrPathRuleInvalid
			}
			suffix := strings.TrimPrefix(strings.TrimPrefix(value[len(token):], "/"), "\\")
			value = filepath.Join(expanded, filepath.FromSlash(suffix))
			break
		}
	}
	if strings.ContainsAny(value, "%$~\x00") || !canonicalAbsolutePath(value) {
		return "", ErrPathRuleInvalid
	}
	return value, nil
}
func pathsOverlapFold(a, b string) bool {
	a = strings.ToLower(a)
	b = strings.ToLower(b)
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
func safePortableRepositoryPath(value string) bool {
	if !safeRelativeStatusPath(value) || strings.Contains(value, "\\") {
		return false
	}
	for _, p := range strings.Split(value, "/") {
		if strings.ContainsAny(p, ":\x00") || strings.TrimRight(p, ". ") != p {
			return false
		}
		name := strings.ToUpper(strings.SplitN(p, ".", 2)[0])
		if name == "CON" || name == "PRN" || name == "AUX" || name == "NUL" || len(name) == 4 && (strings.HasPrefix(name, "COM") || strings.HasPrefix(name, "LPT")) && name[3] >= '1' && name[3] <= '9' {
			return false
		}
	}
	return true
}

// Every existing component must be a real directory/file, never a link. This
// check is repeated immediately before IO, including after parent creation.
func checkSafeAbsolutePath(value string) error {
	if !canonicalAbsolutePath(value) {
		return ErrPathRuleInvalid
	}
	volume := filepath.VolumeName(value)
	current := volume + string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(value, current), string(filepath.Separator))
	for i, part := range parts {
		if part == "" {
			continue
		}
		if !safePortableRepositoryPath(part) {
			return ErrPathRuleInvalid
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() && i < len(parts)-1 {
			return ErrPathRuleInvalid
		}
		if err := checkMappedPlatformPath(current); err != nil {
			return err
		}
		resolved, err := filepath.EvalSymlinks(current)
		if err != nil || !mappedPlatformPathsEqual(resolved, current) {
			return ErrPathRuleInvalid
		}
	}
	return nil
}
func (m *PathMapping) Revision() string { return m.revision }
func (m *PathMapping) LocalPath(name string) (string, bool) {
	if !safePortableRepositoryPath(name) {
		return "", false
	}
	var owner *PathRule
	for i := range m.rules {
		r := &m.rules[i]
		matches := name == r.RepositoryPath && r.Kind == "file" || r.Kind == "directory" && strings.HasPrefix(name, r.RepositoryPath+"/")
		if !matches {
			continue
		}
		if owner == nil || sourceRank(r.Source) > sourceRank(owner.Source) {
			owner = r
		}
	}
	if owner == nil {
		return "", false
	}
	r := *owner
	if r.Kind == "file" {
		return r.LocalPath, true
	}
	suffix := strings.TrimPrefix(name, r.RepositoryPath+"/")
	selected := len(r.Include) == 0
	for _, pattern := range r.Include {
		if yes, _ := doublestar.Match(pattern, suffix); yes {
			selected = true
		}
	}
	for _, pattern := range r.Exclude {
		if yes, _ := doublestar.Match(pattern, suffix); yes {
			selected = false
		}
	}
	if !selected {
		return "", false
	}
	return filepath.Join(r.LocalPath, filepath.FromSlash(suffix)), true
}
func (m *PathMapping) eligible(name, local string, policy RuntimePolicy, manifest Manifest) bool {
	if protectedAbsoluteDestination(local, policy) {
		return false
	}
	relative, err := filepath.Rel(m.home, local)
	if err != nil {
		return false
	}
	physical := filepath.ToSlash(relative)
	if strings.HasPrefix(physical, "../") {
		physical = strings.TrimPrefix(filepath.ToSlash(local), "/")
	}
	ignored := false
	for _, pattern := range manifest.patterns {
		switch pattern.Match(strings.Split(physical, "/"), false) {
		case gitignore.Exclude:
			ignored = true
		case gitignore.Include:
			ignored = false
		}
	}
	return !ignored && manifest.Manages(name, false) && !mandatoryExcluded(name, policy) && !mandatoryExcluded(physical, policy) && !manifestHardExcluded(physical)
}
func (m *PathMapping) Snapshot(policy RuntimePolicy, manifest Manifest, repositoryRoot string, local bool) (Snapshot, error) {
	return m.SnapshotContext(context.Background(), policy, manifest, repositoryRoot, local)
}
func (m *PathMapping) SnapshotContext(ctx context.Context, policy RuntimePolicy, manifest Manifest, repositoryRoot string, local bool) (Snapshot, error) {
	result := Snapshot{Files: map[string]FileState{}}
	var total int64
	seen := map[string]string{}
	add := func(name, source string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		destination, ok := m.LocalPath(name)
		if !ok || local && source != destination || !m.eligible(name, destination, policy, manifest) {
			return nil
		}
		if _, exists := result.Files[name]; exists {
			return nil
		}
		key := strings.ToLower(name)
		if old, ok := seen[key]; ok && old != name {
			return &PathRuleError{Code: "case_collision"}
		}
		seen[key] = name
		if err := checkSafeAbsolutePath(source); err != nil {
			return &PathRuleError{Code: "unsafe_source"}
		}
		info, err := os.Lstat(source)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return &PathRuleError{Code: "unsupported_file_type"}
		}
		if !safeSnapshotPermissions(source, info) {
			return &PathRuleError{Code: "unsafe_permissions"}
		}
		if info.Size() > policy.MaxFileBytes || info.Size() > policy.MaxBatchBytes-total {
			return ErrSnapshotInvalid
		}
		value, opened, err := secureReadFile(source, policy.MaxFileBytes)
		state := FileState{}
		stable := false
		if err == nil {
			state = FileState{Hash: hashSnapshotBytes(value), Bytes: int64(len(value)), Mode: canonicalMappedMode(opened)}
			stable = os.SameFile(info, opened) && info.Size() == opened.Size() && info.ModTime().Equal(opened.ModTime())
		}
		if err != nil {
			return err
		}
		if !stable {
			return ErrSourceChanged
		}
		if len(result.Files) >= policy.ManifestMaxLines {
			return ErrSnapshotInvalid
		}
		result.Files[name] = state
		total += state.Bytes
		return nil
	}
	for _, r := range m.rules {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if protectedAbsoluteDestination(r.LocalPath, policy) {
			continue
		}
		start := r.LocalPath
		if !local {
			start = filepath.Join(repositoryRoot, filepath.FromSlash(r.RepositoryPath))
		}
		if err := checkSafeAbsolutePath(start); err != nil {
			return result, err
		}
		if _, err := os.Lstat(start); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return result, err
		}
		if r.Kind == "file" {
			if err := add(r.RepositoryPath, start); err != nil {
				return result, err
			}
			continue
		}
		info, err := os.Lstat(start)
		if err != nil || !info.IsDir() {
			return result, &PathRuleError{RuleID: r.ID, Code: "directory_required"}
		}
		err = filepath.WalkDir(start, func(full string, entry fs.DirEntry, err error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err != nil {
				return err
			}
			suffix, _ := filepath.Rel(start, full)
			if suffix == "." {
				return nil
			}
			name := r.RepositoryPath + "/" + filepath.ToSlash(suffix)
			if entry.IsDir() {
				dest := filepath.Join(r.LocalPath, suffix)
				if protectedAbsoluteDestination(dest, policy) || ruleExcludesDirectory(r, suffix) || mandatoryExcluded(name, policy) || mandatoryExcluded(strings.TrimPrefix(filepath.ToSlash(dest), "/"), policy) {
					return filepath.SkipDir
				}
				return nil
			}
			return add(name, full)
		})
		if err != nil {
			return result, err
		}
	}
	return result, nil
}
func (m *PathMapping) write(name string, value []byte, mode os.FileMode) error {
	target, ok := m.LocalPath(name)
	if !ok {
		return ErrPathRuleInvalid
	}
	if err := checkSafeAbsolutePath(target); err != nil {
		return err
	}
	if info, err := os.Lstat(target); err == nil && info.Mode().Perm()&0200 == 0 {
		return &PathRuleError{Code: "unwritable_destination"}
	}
	parent := filepath.Dir(target)
	for {
		info, err := os.Stat(parent)
		if err == nil {
			if info.Mode().Perm()&0300 != 0300 {
				return &PathRuleError{Code: "unwritable_destination"}
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return ErrPathRuleInvalid
		}
		parent = next
	}
	if err := secureWriteFile(target, value, mode); err != nil {
		return errors.Join(&PathRuleError{Code: "destination_write_failed"}, err)
	}
	return nil
}
func (m *PathMapping) remove(name string) error {
	target, ok := m.LocalPath(name)
	if !ok {
		return ErrPathRuleInvalid
	}
	if err := checkSafeAbsolutePath(target); err != nil {
		return err
	}
	if err := secureRemoveFile(target); err != nil {
		return errors.Join(&PathRuleError{Code: "destination_delete_failed"}, err)
	}
	return nil
}
func (m *PathMapping) read(name string, state FileState, max int64) ([]byte, error) {
	target, ok := m.LocalPath(name)
	if !ok {
		return nil, ErrPathRuleInvalid
	}
	if err := checkSafeAbsolutePath(target); err != nil {
		return nil, err
	}
	return readVerifiedState(filepath.Dir(target), filepath.Base(target), state, max)
}

func (m *PathMapping) destinations(files map[string]FileState) map[string]string {
	result := map[string]string{}
	for name := range files {
		if target, ok := m.LocalPath(name); ok {
			result[name] = target
		}
	}
	return result
}

func protectedAbsoluteDestination(local string, policy RuntimePolicy) bool {
	for _, root := range policy.AbsoluteRuntimeExclusionRoots {
		if sameOrInsidePath(local, root) {
			return true
		}
	}
	return false
}
func ruleExcludesDirectory(rule PathRule, suffix string) bool {
	for _, pattern := range rule.Exclude {
		if yes, _ := doublestar.Match(pattern, suffix); yes {
			return true
		}
		if strings.HasSuffix(pattern, "/**") {
			if yes, _ := doublestar.Match(pattern, suffix+"/__scope_probe"); yes {
				return true
			}
		}
	}
	return false
}

// Git records executable intent, not per-machine POSIX permission bits. Keep
// local permissions on replacement while comparing a portable private mode.
func canonicalMappedMode(info os.FileInfo) os.FileMode {
	if snapshotFileMode(info)&0111 != 0 {
		return 0700
	}
	return 0600
}

func (m *PathMapping) RepositoryPath(full string) (string, bool) {
	for _, rule := range m.rules {
		if rule.Kind == "file" && full == rule.LocalPath {
			return rule.RepositoryPath, true
		}
		if rule.Kind == "directory" && sameOrInsidePath(full, rule.LocalPath) {
			suffix, err := filepath.Rel(rule.LocalPath, full)
			if err != nil || suffix == "." {
				continue
			}
			name := rule.RepositoryPath + "/" + filepath.ToSlash(suffix)
			if target, ok := m.LocalPath(name); ok && target == full {
				return name, true
			}
		}
	}
	return "", false
}

func sourceRank(source string) int {
	switch source {
	case "machine":
		return 3
	case "os":
		return 2
	case "shared":
		return 1
	}
	return 0
}
