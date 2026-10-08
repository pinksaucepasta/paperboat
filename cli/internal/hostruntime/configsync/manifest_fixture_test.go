package configsync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"sort"
	"strings"
	"unicode/utf8"
)

func ParseManifest(include, ignore []byte, limits ManifestLimits) (Manifest, error) {
	if !validManifestLimits(limits) || len(include) > limits.MaxBytes || len(ignore) > limits.MaxBytes ||
		!utf8.Valid(include) || !utf8.Valid(ignore) || bytes.IndexByte(include, 0) >= 0 || bytes.IndexByte(ignore, 0) >= 0 {
		return Manifest{}, ErrManifestInvalid
	}
	roots, err := parseInclude(include, limits)
	if err != nil {
		return Manifest{}, err
	}
	patterns, err := parseIgnore(ignore, limits)
	if err != nil {
		return Manifest{}, err
	}
	hash := sha256.New()
	_, _ = hash.Write(include)
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(ignore)
	return Manifest{Revision: hex.EncodeToString(hash.Sum(nil)), Roots: roots, patterns: patterns}, nil
}

func parseInclude(value []byte, limits ManifestLimits) ([]ManifestRoot, error) {
	lines, err := manifestLines(value, limits)
	if err != nil {
		return nil, err
	}
	byPath := make(map[string]ManifestRoot)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		directory := strings.HasSuffix(line, "/")
		line = strings.TrimSuffix(line, "/")
		if strings.ContainsAny(line, "*?[") || !safeManifestPath(line) {
			return nil, fmt.Errorf("%w: %q", ErrManifestUnsafePath, line)
		}
		if manifestHardExcluded(line) {
			return nil, fmt.Errorf("%w: %q", ErrManifestUnsafePath, line)
		}
		if previous, exists := byPath[line]; !exists || directory && !previous.Directory {
			byPath[line] = ManifestRoot{Path: line, Directory: directory}
		}
	}
	roots := make([]ManifestRoot, 0, len(byPath))
	for _, root := range byPath {
		roots = append(roots, root)
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].Path < roots[j].Path })
	result := roots[:0]
	for _, candidate := range roots {
		redundant := false
		for _, root := range result {
			if root.Directory && strings.HasPrefix(candidate.Path, root.Path+"/") {
				redundant = true
				break
			}
		}
		if !redundant {
			result = append(result, candidate)
		}
	}
	return result, nil
}

func parseIgnore(value []byte, limits ManifestLimits) ([]gitignore.Pattern, error) {
	lines, err := manifestLines(value, limits)
	if err != nil {
		return nil, err
	}
	patterns := make([]gitignore.Pattern, 0, len(lines))
	for _, line := range lines {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		candidate := strings.TrimPrefix(line, "!")
		candidate = strings.TrimPrefix(candidate, "/")
		candidate = strings.TrimSuffix(candidate, "/")
		if candidate == "" || !safeIgnorePattern(candidate) {
			return nil, ErrManifestInvalid
		}
		patterns = append(patterns, gitignore.ParsePattern(line, nil))
	}
	return patterns, nil
}

func manifestLines(value []byte, limits ManifestLimits) ([]string, error) {
	value = bytes.ReplaceAll(value, []byte("\r\n"), []byte("\n"))
	if bytes.IndexByte(value, '\r') >= 0 {
		return nil, ErrManifestInvalid
	}
	lines := strings.Split(string(value), "\n")
	if len(lines) > limits.MaxLines+1 || len(lines) == limits.MaxLines+1 && lines[len(lines)-1] != "" {
		return nil, ErrManifestInvalid
	}
	for _, line := range lines {
		if len(line) > limits.MaxPatternBytes {
			return nil, ErrManifestInvalid
		}
	}
	return lines, nil
}
