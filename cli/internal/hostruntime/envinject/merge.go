package envinject

import (
	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

func Merge(base, managed []string) ([]string, error) {
	values := make(map[string]string, len(base)+len(managed))
	canonicalNames := make(map[string]string, len(base)+len(managed))
	for _, entry := range base {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !portableLaunchName(name) || strings.ContainsRune(value, '\x00') || !utf8.ValidString(value) {
			return nil, ErrInvalidSnapshot
		}
		folded := strings.ToUpper(name)
		if prior := canonicalNames[folded]; prior != "" && prior != name {
			return nil, ErrInvalidSnapshot
		}
		canonicalNames[folded] = name
		values[name] = value
	}
	managedNames := make(map[string]struct{}, len(managed))
	for _, entry := range managed {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || env.ValidateVariableName(name) != nil || len(value) > env.MaximumValueBytes || strings.ContainsRune(value, '\x00') || !utf8.ValidString(value) {
			return nil, ErrInvalidSnapshot
		}
		outputName := name
		folded := strings.ToUpper(name)
		if _, duplicate := managedNames[folded]; duplicate {
			return nil, ErrInvalidSnapshot
		}
		managedNames[folded] = struct{}{}
		if existing := canonicalNames[folded]; existing != "" {
			outputName = existing
		}
		canonicalNames[folded] = outputName
		values[outputName] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result, nil
}

var launchNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func portableLaunchName(name string) bool {
	return len(name) > 0 && len(name) <= env.MaximumNameBytes && launchNamePattern.MatchString(name)
}
