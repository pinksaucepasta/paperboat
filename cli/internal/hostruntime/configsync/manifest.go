package configsync

import (
	"errors"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

const (
	ManifestContractVersion        = "paperboat-manifest-v1"
	DefaultManifestMaxBytes        = 256 << 10
	DefaultManifestMaxLines        = 4096
	DefaultManifestMaxPatternBytes = 1024
)

var (
	ErrManifestMissing    = errors.New("config manifest missing")
	ErrManifestInvalid    = errors.New("config manifest invalid")
	ErrManifestUnsafePath = errors.New("config manifest contains unsafe path")
)

type ManifestLimits struct {
	MaxBytes        int
	MaxLines        int
	MaxPatternBytes int
}

type ManifestRoot struct {
	Path      string `json:"path"`
	Directory bool   `json:"directory"`
}

type Manifest struct {
	Revision string
	Roots    []ManifestRoot
	patterns []gitignore.Pattern
}

func (m Manifest) Clone() Manifest {
	return Manifest{
		Revision: m.Revision,
		Roots:    append([]ManifestRoot(nil), m.Roots...),
		patterns: append([]gitignore.Pattern(nil), m.patterns...),
	}
}

func DefaultManifestLimits() ManifestLimits {
	return ManifestLimits{
		MaxBytes: DefaultManifestMaxBytes, MaxLines: DefaultManifestMaxLines,
		MaxPatternBytes: DefaultManifestMaxPatternBytes,
	}
}

func (m Manifest) Manages(name string, isDir bool) bool {
	name = path.Clean(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || !safeManifestPath(name) || manifestHardExcluded(name) {
		return false
	}
	selected := false
	for _, root := range m.Roots {
		if name == root.Path || root.Directory && strings.HasPrefix(name, root.Path+"/") {
			selected = true
			break
		}
	}
	if !selected {
		return false
	}
	parts := strings.Split(name, "/")
	ignored := false
	for _, pattern := range m.patterns {
		switch pattern.Match(parts, isDir) {
		case gitignore.Exclude:
			ignored = true
		case gitignore.Include:
			ignored = false
		}
	}
	return !ignored
}

func (m Manifest) MayManageDescendant(name string) bool {
	name = path.Clean(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || !safeManifestPath(name) || manifestHardExcluded(name) {
		return false
	}
	for _, root := range m.Roots {
		if strings.HasPrefix(root.Path, name+"/") || root.Directory && (root.Path == name || strings.HasPrefix(name, root.Path+"/")) {
			return true
		}
	}
	return false
}

func validManifestLimits(limits ManifestLimits) bool {
	return limits.MaxBytes > 0 && limits.MaxBytes <= 4<<20 && limits.MaxLines > 0 && limits.MaxLines <= 65536 &&
		limits.MaxPatternBytes > 0 && limits.MaxPatternBytes <= 8192
}

func safeManifestPath(name string) bool {
	if name == "" || name != path.Clean(name) || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "//") ||
		strings.Contains(name, "\\") || strings.ContainsRune(name, 0) || name == ".." || strings.HasPrefix(name, "../") ||
		strings.Contains(name, "/../") || filepath.IsAbs(name) {
		return false
	}
	first := strings.Split(name, "/")[0]
	return len(first) < 2 || first[1] != ':'
}

func safeIgnorePattern(pattern string) bool {
	if strings.ContainsRune(pattern, 0) || strings.Contains(pattern, "\\") || strings.HasPrefix(pattern, "//") {
		return false
	}
	for _, component := range strings.Split(pattern, "/") {
		if component == ".." {
			return false
		}
		if component == "**" {
			continue
		}
		if _, err := path.Match(component, component); err != nil {
			return false
		}
	}
	return true
}

func manifestHardExcluded(name string) bool {
	return mandatoryExcluded(path.Clean(name), RuntimePolicy{})
}
