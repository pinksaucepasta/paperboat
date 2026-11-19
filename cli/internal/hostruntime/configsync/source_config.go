package configsync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bmatcuk/doublestar/v4"
	"github.com/tailscale/hujson"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
)

const SharedSourceConfigPath = ".paperboat/config-sync.jsonc"

var ErrSourceConfigInvalid = errors.New("invalid config sync source configuration")
var ErrMachineNameUnavailable = errors.New("config sync shared file has [machine.<name>] sections but this machine's Paperboat name is not known yet; update pb on every machine and run pb config sync apply again")
var ErrConfigurationChanged = errors.New("config sync configuration changed; run pb config sync apply to approve the current files")

type RepositoryTarget struct {
	RepositoryID string `json:"repository_id" required:"true" minLength:"1" maxLength:"1024" doc:"The registered config sync repository to use: its id or name (pb config repository list)." examples:"[\"dotfiles\"]"`
	URL          string `json:"url,omitempty" maxLength:"4096" doc:"The repository's Git URL. Normally left out: the registered repository's own URL is used." examples:"[\"https://git.example.test/me/dotfiles.git\"]"`
	Branch       string `json:"branch,omitempty" maxLength:"1024" doc:"The Git branch to sync. Default: the repository's branch." examples:"[\"main\"]"`
}
type SourceConfig struct {
	Schema           string                  `json:"$schema,omitempty" doc:"The JSON Schema of this file, so editors check and complete it. Ignored by Paperboat." examples:"[\"https://example.test/schemas/config-sync.json\"]"`
	OS               map[string]SourceConfig `json:"os,omitempty" keys:"^(linux|darwin|windows)$" doc:"Settings for one operating system, keyed linux, darwin or windows. They override the file's top-level settings on that system. A section holds the same keys, except os and machine." examples:"[{\"windows\": {\"paths\": {\"editor/settings.json\": {\"local_path\": \"%APPDATA%/editor/settings.json\"}}}}]"`
	Machine          map[string]SourceConfig `json:"machine,omitempty" keys:"^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$" doc:"Settings for one machine, keyed by its Paperboat name. They override the os section. Only in the shared file in the repository, never in a machine's own file. A section holds the same keys, except os and machine." examples:"[{\"laptop\": {\"mode\": \"pull_only\"}}]"`
	Enabled          *bool                   `json:"enabled,omitempty" default:"true" doc:"Whether config sync runs on this machine." examples:"[false]"`
	Pull             *RepositoryTarget       `json:"pull,omitempty" doc:"The repository this machine pulls from." examples:"[{\"repository_id\": \"dotfiles\"}]"`
	Push             *RepositoryTarget       `json:"push,omitempty" doc:"The repository this machine pushes to. Default: the pull repository." examples:"[{\"repository_id\": \"dotfiles\", \"branch\": \"laptop\"}]"`
	Version          *int                    `json:"version,omitempty" minimum:"1" maximum:"1" doc:"The file format version. Only 1 exists." default:"1" examples:"[1]"`
	Mode             *AssignmentMode         `json:"mode,omitempty" ref:"mode" doc:"Which way files move: pull_only, push_only or bidirectional." default:"\"bidirectional\"" examples:"[\"pull_only\"]"`
	AutomaticUpdates *bool                   `json:"automatic_updates,omitempty" default:"false" doc:"Whether changes found in the repository are applied without asking." examples:"[true]"`
	Paths            map[string]RuleConfig   `json:"paths,omitempty" keys:"^[^\\\\\\x00]+$" doc:"The files and directories that sync, keyed by their path inside the repository, like editor/settings.json: a relative path with / separators, no .. and no Windows reserved names. Two paths may not contain one another." examples:"[{\"shell/zshrc\": {\"local_path\": \"~/.zshrc\", \"kind\": \"file\"}}]"`
}
type RuleConfig struct {
	LocalPath *string   `json:"local_path,omitempty" minLength:"1" maxLength:"4096" doc:"Where the path lives on this machine: absolute, or starting with ~ or %APPDATA%. Destinations are never inferred." examples:"[\"~/.config/editor\", \"%APPDATA%/editor/settings.json\"]"`
	Kind      *string   `json:"kind,omitempty" ref:"kind" doc:"file or directory." examples:"[\"directory\"]"`
	Include   *[]string `json:"include,omitempty" maxItems:"64" minLength:"1" maxLength:"1024" doc:"For a directory: only files matching one of these patterns sync (** matches across directories). Patterns are relative, use /, and contain no .." examples:"[[\"*.json\", \"snippets/**\"]]"`
	Exclude   *[]string `json:"exclude,omitempty" maxItems:"64" minLength:"1" maxLength:"1024" doc:"Files matching one of these patterns never sync. Same pattern rules as include." examples:"[[\"**/*.tmp\", \"private/**\"]]"`
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
	standard, err := hujson.Standardize(bytes.Clone(data)) // Standardize blanks comments in the buffer it is given
	if err != nil {
		if _, emptyErr := hujson.Standardize(append(data[:len(data):len(data)], "\n{}"...)); emptyErr == nil {
			return SourceConfig{}, nil // only comments: nothing is configured
		}
		return SourceConfig{}, fmt.Errorf("%w: %v", ErrSourceConfigInvalid, err)
	}
	var raw any
	if err := json.Unmarshal(standard, &raw); err != nil {
		return SourceConfig{}, fmt.Errorf("%w: %v", ErrSourceConfigInvalid, err)
	}
	var config SourceConfig
	// encoding/json matches keys without regard to case; the file's keys are exact.
	if err := exactKeys(raw, reflect.TypeOf(config), ""); err != nil {
		return SourceConfig{}, fmt.Errorf("%w: %v", ErrSourceConfigInvalid, err)
	}
	if err := json.Unmarshal(standard, &config); err != nil {
		return SourceConfig{}, fmt.Errorf("%w: %v", ErrSourceConfigInvalid, err)
	}
	if err := validateSourceConfig(config, limits); err != nil {
		return SourceConfig{}, err
	}
	return config, nil
}

// exactKeys rejects an object key that is not a field of t, spelled exactly as its json tag.
func exactKeys(value any, t reflect.Type, path string) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if value == nil {
		return fmt.Errorf("%s is null; leave the key out instead", path)
	}
	object, isObject := value.(map[string]any)
	if !isObject {
		return nil // the decoder reports a value of the wrong type
	}
	switch t.Kind() {
	case reflect.Map:
		for key, child := range object {
			if err := exactKeys(child, t.Elem(), path+"/"+key); err != nil {
				return err
			}
		}
	case reflect.Struct:
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
			fields[name] = t.Field(i).Type
		}
		for key, child := range object {
			field, known := fields[key]
			if !known {
				return fmt.Errorf("unknown key %q at %s", key, path+"/"+key)
			}
			if err := exactKeys(child, field, path+"/"+key); err != nil {
				return err
			}
		}
	}
	return nil
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
		if platform != "linux" && platform != "darwin" && platform != "windows" || section.OS != nil || section.Machine != nil {
			return ErrSourceConfigInvalid
		}
		if err := validateSourceConfig(section, limits); err != nil {
			return err
		}
	}
	for name, section := range config.Machine {
		if !validMachineName(name) || section.OS != nil || section.Machine != nil {
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

// validMachineName accepts the syntax of a Paperboat machine name: lowercase
// letters, digits and hyphens, 1-63 characters, no leading or trailing hyphen.
func validMachineName(name string) bool {
	if name == "" || len(name) > 63 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for _, character := range name {
		if character != '-' && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

// MergeSourceConfigs layers the shared file and this machine's own file.
// machineName is this machine's Paperboat name; it selects the shared file's
// [machine.<name>] section and may be empty only when the file has none.
func MergeSourceConfigs(shared, machine SourceConfig, machineName string) (EffectiveConfig, error) {
	return MergeSourceConfigsForOS(shared, machine, runtime.GOOS, machineName)
}

// MergeSourceConfigsForOS layers, lowest to highest: shared, [os.<goos>],
// [machine.<machineName>], then the machine-local file.
func MergeSourceConfigsForOS(shared, machine SourceConfig, goos, machineName string) (EffectiveConfig, error) {
	limits := DefaultSourceConfigLimits()
	if err := validateSourceConfig(shared, limits); err != nil {
		return EffectiveConfig{}, err
	}
	if err := validateSourceConfig(machine, limits); err != nil {
		return EffectiveConfig{}, err
	}
	if len(machine.OS) > 0 || len(machine.Machine) > 0 {
		return EffectiveConfig{}, ErrSourceConfigInvalid
	}
	if machineName == "" && len(shared.Machine) > 0 {
		return EffectiveConfig{}, ErrMachineNameUnavailable
	}
	if machineName != "" && !validMachineName(machineName) {
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
	}{{"shared", shared}, {"os", shared.OS[goos]}, {"machine_section", shared.Machine[machineName]}, {"machine", machine}} {
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
