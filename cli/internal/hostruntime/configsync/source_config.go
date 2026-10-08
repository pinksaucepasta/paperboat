package configsync

import (
	"encoding/json"
	"errors"
	"github.com/BurntSushi/toml"
	"github.com/bmatcuk/doublestar/v4"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const SharedSourceConfigPath = ".paperboat/config-sync.toml"

var ErrSourceConfigInvalid = errors.New("invalid config sync source configuration")
var ErrConfigurationChanged = errors.New("config sync configuration changed; run pb config sync apply to approve the current files")

type RepositoryTarget struct {
	RepositoryID string `json:"repository_id" toml:"repository_id"`
	URL          string `json:"url,omitempty" toml:"url"`
	Branch       string `json:"branch,omitempty" toml:"branch"`
}
type SourceConfig struct {
	OS               map[string]SourceConfig `json:"os,omitempty" toml:"os"`
	Enabled          *bool                   `json:"enabled,omitempty" toml:"enabled"`
	Pull             *RepositoryTarget       `json:"pull,omitempty" toml:"pull"`
	Push             *RepositoryTarget       `json:"push,omitempty" toml:"push"`
	Version          *int                    `json:"version,omitempty" toml:"version"`
	Mode             *AssignmentMode         `json:"mode,omitempty" toml:"mode"`
	AutomaticUpdates *bool                   `json:"automatic_updates,omitempty" toml:"automatic_updates"`
	Paths            map[string]RuleConfig   `json:"paths,omitempty" toml:"paths"`
}
type RuleConfig struct {
	LocalPath *string   `json:"local_path,omitempty" toml:"local_path"`
	Kind      *string   `json:"kind,omitempty" toml:"kind"`
	Include   *[]string `json:"include,omitempty" toml:"include"`
	Exclude   *[]string `json:"exclude,omitempty" toml:"exclude"`
}
type SourceConfigLimits struct{ MaxBytes, MaxPaths, MaxPatterns, MaxPatternBytes int }

func DefaultSourceConfigLimits() SourceConfigLimits {
	return SourceConfigLimits{MaxBytes: 256 << 10, MaxPaths: 256, MaxPatterns: 64, MaxPatternBytes: 1024}
}

type EffectiveConfig struct {
	Enabled          bool
	Pull, Push       *RepositoryTarget
	Mode             AssignmentMode
	AutomaticUpdates bool
	PathRules        []PathRule
	Revision         string
}

// LoadSourceConfig treats only absence as an empty source. A present invalid,
// linked or unreadable source never falls back to another source.
func LoadSourceConfig(path string, limits SourceConfigLimits) (SourceConfig, error) {
	if !canonicalAbsolutePath(path) {
		return SourceConfig{}, ErrSourceConfigInvalid
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return SourceConfig{}, nil
	} else if err != nil {
		return SourceConfig{}, errors.Join(ErrSourceConfigInvalid, err)
	}
	if err := checkSafeAbsolutePath(path); err != nil {
		return SourceConfig{}, errors.Join(ErrSourceConfigInvalid, err)
	}
	data, _, err := secureReadFile(path, int64(limits.MaxBytes))
	if err != nil {
		return SourceConfig{}, errors.Join(ErrSourceConfigInvalid, err)
	}
	return ParseSourceConfig(data, limits)
}
func ParseSourceConfig(data []byte, limits SourceConfigLimits) (SourceConfig, error) {
	if limits.MaxBytes < 1 || limits.MaxPaths < 1 || limits.MaxPatterns < 1 || limits.MaxPatternBytes < 1 || len(data) > limits.MaxBytes {
		return SourceConfig{}, ErrSourceConfigInvalid
	}
	var config SourceConfig
	metadata, err := toml.Decode(string(data), &config)
	if err != nil || len(metadata.Undecoded()) != 0 {
		return SourceConfig{}, ErrSourceConfigInvalid
	}
	for _, key := range metadata.Keys() {
		parts := []string(key)
		if parts[0] == "os" && len(parts) >= 3 {
			parts = parts[2:]
		}
		expected := "Hash"
		switch parts[0] {
		case "version":
			expected = "Integer"
		case "enabled", "automatic_updates":
			expected = "Bool"
		case "mode":
			expected = "String"
		case "pull", "push":
			if len(parts) > 1 {
				if len(parts) != 2 || (parts[1] != "repository_id" && parts[1] != "url" && parts[1] != "branch") {
					return SourceConfig{}, ErrSourceConfigInvalid
				}
				expected = "String"
			}
		case "paths":
			if len(parts) > 2 {
				if len(parts) != 3 || (parts[2] != "local_path" && parts[2] != "kind" && parts[2] != "include" && parts[2] != "exclude") {
					return SourceConfig{}, ErrSourceConfigInvalid
				}
				expected = "String"
				if parts[2] == "include" || parts[2] == "exclude" {
					expected = "Array"
				}
			}
		case "os":
		default:
			return SourceConfig{}, ErrSourceConfigInvalid
		}
		if metadata.Type(key...) != expected {
			return SourceConfig{}, ErrSourceConfigInvalid
		}
	}
	if err := validateSourceConfig(config, limits); err != nil {
		return SourceConfig{}, err
	}
	return config, nil
}
func validateSourceConfig(config SourceConfig, limits SourceConfigLimits) error {
	if config.Version != nil && *config.Version != 1 || config.Mode != nil && !config.Mode.Valid() || len(config.Paths) > limits.MaxPaths {
		return ErrSourceConfigInvalid
	}
	for _, target := range []*RepositoryTarget{config.Pull, config.Push} {
		if target != nil && (target.RepositoryID == "" || len(target.RepositoryID) > 1024 || len(target.URL) > 4096 || len(target.Branch) > 1024 || strings.ContainsAny(target.RepositoryID+target.URL+target.Branch, "\x00\r\n")) {
			return ErrSourceConfigInvalid
		}
	}
	for platform, section := range config.OS {
		if platform != "linux" && platform != "darwin" && platform != "windows" || section.OS != nil {
			return ErrSourceConfigInvalid
		}
		if err := validateSourceConfig(section, limits); err != nil {
			return err
		}
	}
	keys := make([]string, 0, len(config.Paths))
	for name, rule := range config.Paths {
		if !safePortableRepositoryPath(name) || manifestHardExcluded(name) {
			return ErrSourceConfigInvalid
		}
		if rule.Kind != nil && *rule.Kind != "file" && *rule.Kind != "directory" {
			return ErrSourceConfigInvalid
		}
		if rule.LocalPath != nil && (*rule.LocalPath == "" || len(*rule.LocalPath) > 4096 || strings.ContainsAny(*rule.LocalPath, "\x00\r\n")) {
			return ErrSourceConfigInvalid
		}
		count := 0
		for _, patterns := range []*[]string{rule.Include, rule.Exclude} {
			if patterns == nil {
				continue
			}
			count += len(*patterns)
			for _, pattern := range *patterns {
				if !doublestar.ValidatePattern(pattern) || len(pattern) > limits.MaxPatternBytes || pattern == "" || strings.Contains(pattern, "\\") || strings.HasPrefix(pattern, "/") || strings.Contains(pattern, "..") {
					return ErrSourceConfigInvalid
				}
			}
		}
		if count > limits.MaxPatterns {
			return ErrSourceConfigInvalid
		}
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for i, name := range keys {
		for _, other := range keys[:i] {
			if pathsOverlapFold(name, other) {
				return ErrSourceConfigInvalid
			}
		}
	}
	return nil
}
func MergeSourceConfigs(shared, machine SourceConfig) (EffectiveConfig, error) {
	return MergeSourceConfigsForOS(shared, machine, runtime.GOOS)
}
func MergeSourceConfigsForOS(shared, machine SourceConfig, goos string) (EffectiveConfig, error) {
	limits := DefaultSourceConfigLimits()
	if err := validateSourceConfig(shared, limits); err != nil {
		return EffectiveConfig{}, err
	}
	if err := validateSourceConfig(machine, limits); err != nil {
		return EffectiveConfig{}, err
	}
	if len(machine.OS) > 0 {
		return EffectiveConfig{}, ErrSourceConfigInvalid
	}
	if goos != "linux" && goos != "darwin" && goos != "windows" {
		return EffectiveConfig{}, ErrSourceConfigInvalid
	}
	config := EffectiveConfig{Enabled: true, Mode: ModeBidirectional, AutomaticUpdates: false, PathRules: []PathRule{}}
	rules := map[string]RuleConfig{}
	origins := map[string]string{}
	for _, layer := range []struct {
		source string
		config SourceConfig
	}{{"shared", shared}, {"os", shared.OS[goos]}, {"machine", machine}} {
		c := layer.config
		if c.Pull != nil {
			target := *c.Pull
			config.Pull = &target
		}
		if c.Push != nil {
			target := *c.Push
			config.Push = &target
		}
		if c.Enabled != nil {
			config.Enabled = *c.Enabled
		}
		if c.Mode != nil {
			config.Mode = *c.Mode
		}
		if c.AutomaticUpdates != nil {
			config.AutomaticUpdates = *c.AutomaticUpdates
		}
		for name, rule := range c.Paths {
			rules[name] = mergeRulePresence(rules[name], rule)
			origins[name] = layer.source
		}
	}
	if len(rules) > limits.MaxPaths {
		return EffectiveConfig{}, ErrSourceConfigInvalid
	}
	for name, rule := range rules {
		if rule.LocalPath == nil || rule.Kind == nil {
			return EffectiveConfig{}, ErrSourceConfigInvalid
		}
		pathRule := PathRule{ID: origins[name] + ":" + name, Source: origins[name], RepositoryPath: name, LocalPath: *rule.LocalPath, Kind: *rule.Kind, Include: []string{}, Exclude: []string{}}
		if rule.Include != nil {
			pathRule.Include = append(pathRule.Include, (*rule.Include)...)
		}
		if rule.Exclude != nil {
			pathRule.Exclude = append(pathRule.Exclude, (*rule.Exclude)...)
		}
		if pathRule.Kind == "file" && len(pathRule.Include)+len(pathRule.Exclude) > 0 {
			return EffectiveConfig{}, ErrSourceConfigInvalid
		}
		config.PathRules = append(config.PathRules, pathRule)
	}
	sort.Slice(config.PathRules, func(i, j int) bool { return config.PathRules[i].RepositoryPath < config.PathRules[j].RepositoryPath })
	config.Revision = EffectiveConfigRevision(config)
	return config, nil
}
func mergeRulePresence(base, override RuleConfig) RuleConfig {
	if override.LocalPath != nil {
		base.LocalPath = override.LocalPath
	}
	if override.Kind != nil {
		base.Kind = override.Kind
	}
	if override.Include != nil {
		base.Include = override.Include
	}
	if override.Exclude != nil {
		base.Exclude = override.Exclude
	}
	return base
}
func ProjectionRevision(mode AssignmentMode, automatic bool, rules []PathRule) string {
	copyRules := append([]PathRule{}, rules...)
	sort.Slice(copyRules, func(i, j int) bool { return copyRules[i].RepositoryPath < copyRules[j].RepositoryPath })
	for i := range copyRules {
		copyRules[i].ID = ""
		if copyRules[i].Include == nil {
			copyRules[i].Include = []string{}
		}
		if copyRules[i].Exclude == nil {
			copyRules[i].Exclude = []string{}
		}
	}
	data, _ := json.Marshal(struct {
		Mode      AssignmentMode
		Automatic bool
		Rules     []PathRule
	}{mode, automatic, copyRules})
	return hashSnapshotBytes(data)
}
func ResolveEffectiveConfig(home string, config EffectiveConfig) (*PathMapping, error) {
	return ResolvePathRules(home, config.PathRules)
}
func (c EffectiveConfig) Manifest() Manifest {
	roots := make([]ManifestRoot, 0, len(c.PathRules))
	for _, rule := range c.PathRules {
		roots = append(roots, ManifestRoot{Path: rule.RepositoryPath, Directory: rule.Kind == "directory"})
	}
	return Manifest{Revision: c.Revision, Roots: roots}
}
func LoadSharedSourceConfig(repositoryRoot string, limits SourceConfigLimits) (SourceConfig, error) {
	return LoadSourceConfig(filepath.Join(repositoryRoot, filepath.FromSlash(SharedSourceConfigPath)), limits)
}

func EffectiveConfigRevision(config EffectiveConfig) string {
	data, _ := json.Marshal(struct {
		Projection string
		Enabled    bool
		Pull, Push *RepositoryTarget
	}{ProjectionRevision(config.Mode, config.AutomaticUpdates, config.PathRules), config.Enabled, config.Pull, config.Push})
	return hashSnapshotBytes(data)
}
