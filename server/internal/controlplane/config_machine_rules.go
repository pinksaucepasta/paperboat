package controlplane

import (
	"encoding/json"
	"net/url"
	"path"
	"strings"
	"unicode"
)

// ConfigPathRule grants a machine an explicit mapping. Repository data cannot
// supply or expand the destination; the worker verifies native filesystem safety.
type ConfigPathRule struct {
	ID             string   `json:"id"`
	Source         string   `json:"source"`
	RepositoryPath string   `json:"repository_path"`
	LocalPath      string   `json:"local_path"`
	Kind           string   `json:"kind"`
	Include        []string `json:"include"`
	Exclude        []string `json:"exclude"`
}
type ConfigRepositoryBinding struct {
	RepositoryID string `json:"repository_id"`
	URL          string `json:"url"`
}

func safeCoordinateText(s string) bool {
	return s != "" && len(s) <= 4096 && !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) }) && strings.TrimSpace(s) == s
}

// ConfigRepositoryTransport recognizes explicit endpoints without contacting them.
// SSH usernames identify the remote account; URL passwords and query credentials
// are never accepted. Local paths belong to the assigned endpoint's namespace.
func ConfigRepositoryTransport(raw string) (string, bool) {
	if !safeCoordinateText(raw) {
		return "", false
	}
	if nativeAbsolute(raw) {
		return "local", !hasTraversal(strings.ReplaceAll(raw, "\\", "/")) && !strings.ContainsAny(raw, "?#")
	}
	if !strings.Contains(raw, "://") {
		colon := strings.IndexByte(raw, ':')
		if colon > 0 && colon < len(raw)-1 && !strings.ContainsAny(raw[:colon], "/\\?#") {
			host := raw[:colon]
			if at := strings.LastIndexByte(host, '@'); at >= 0 {
				if at == 0 {
					return "", false
				}
				host = host[at+1:]
			}
			return "ssh", host != "" && !strings.ContainsAny(raw[colon+1:], "?#") && !hasTraversal(raw[colon+1:])
		}
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.Path == "" || hasTraversal(u.Path) {
		return "", false
	}
	switch u.Scheme {
	case "https", "http":
		return u.Scheme, u.Hostname() != "" && u.User == nil
	case "ssh":
		if u.User != nil {
			if _, password := u.User.Password(); password || u.User.Username() == "" {
				return "", false
			}
		}
		return "ssh", u.Hostname() != ""
	case "file":
		return "local", u.User == nil && (u.Host == "" || u.Host == "localhost") && strings.HasPrefix(u.Path, "/")
	}
	return "", false
}
func nativeAbsolute(s string) bool {
	return strings.HasPrefix(s, "/") || (len(s) > 2 && ((s[0] >= 'A' && s[0] <= 'Z') || (s[0] >= 'a' && s[0] <= 'z')) && s[1] == ':' && (s[2] == '\\' || s[2] == '/')) || strings.HasPrefix(s, "\\\\")
}

// The server stores explicit expansion tokens unchanged. Only the owning machine
// may resolve their values from its native user context.
func validConfigDestination(s, kind string) bool {
	if !safeCoordinateText(s) || hasTraversal(strings.ReplaceAll(s, "\\", "/")) {
		return false
	}
	if nativeAbsolute(s) {
		return !strings.ContainsAny(s, "%$~")
	}
	for _, prefix := range []string{"~", "$XDG_CONFIG_HOME", "%APPDATA%", "%LOCALAPPDATA%", "%USERPROFILE%"} {
		if s == prefix {
			return kind == "directory"
		}
		if strings.HasPrefix(s, prefix+"/") || strings.HasPrefix(s, prefix+"\\") {
			return !strings.ContainsAny(strings.TrimPrefix(s, prefix), "%$~")
		}
	}
	return false
}
func hasTraversal(s string) bool {
	for _, part := range strings.Split(s, "/") {
		if part == ".." || part == "." {
			return true
		}
	}
	return false
}
func validRepositoryPath(s string) bool {
	return safeCoordinateText(s) && !strings.ContainsAny(s, "\\:*?[]") && !strings.HasPrefix(s, "/") && path.Clean(s) == s && s != ".." && !strings.HasPrefix(s, "../")
}
func validSelector(s string) bool {
	if !safeCoordinateText(s) || strings.Contains(s, "\\") || strings.HasPrefix(s, "/") || hasTraversal(s) {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" {
			return false
		}
		if _, err := path.Match(part, ""); err != nil {
			return false
		}
	}
	return true
}

// validConfigRuleSource lists where an effective rule came from, lowest priority
// first: the shared file, its [os.<os>] section, its [machine.<name>] section and
// the machine's own file.
func validConfigRuleSource(source string) bool {
	switch source {
	case "shared", "os", "machine_section", "machine":
		return true
	}
	return false
}
func ValidateConfigMachineRules(rules []ConfigPathRule, bindings []ConfigRepositoryBinding, pull, push string) error {
	if len(rules) > 256 || len(bindings) > 2 {
		return ErrAssignmentInvalid
	}
	ids := map[string]bool{}
	for _, r := range rules {
		if !validConfigRuleSource(r.Source) || !safeCoordinateText(r.ID) || len(r.ID) > 128 || ids[r.ID] || !validRepositoryPath(r.RepositoryPath) || !validConfigDestination(r.LocalPath, r.Kind) || (r.Kind != "file" && r.Kind != "directory") || len(r.Include)+len(r.Exclude) > 64 {
			return ErrAssignmentInvalid
		}
		if r.RepositoryPath == "." && r.Kind != "directory" {
			return ErrAssignmentInvalid
		}
		ids[r.ID] = true
		if r.Kind == "file" && len(r.Include)+len(r.Exclude) > 0 {
			return ErrAssignmentInvalid
		}
		for _, patterns := range [][]string{r.Include, r.Exclude} {
			for _, pattern := range patterns {
				if !validSelector(pattern) {
					return ErrAssignmentInvalid
				}
			}
		}
	}

	for i, a := range rules {
		for _, b := range rules[i+1:] {
			if a.Source == b.Source && configCoordinatesOverlap(a.RepositoryPath, b.RepositoryPath) {
				return ErrAssignmentInvalid
			}
			// Effective rules must own distinct destinations, including across sources.
			if configCoordinatesOverlap(configDestinationCoordinate(a.LocalPath), configDestinationCoordinate(b.LocalPath)) && !configNaturalNestedOverride(a, b) {
				return ErrAssignmentInvalid
			}
		}
	}
	seen := map[string]bool{}
	for _, b := range bindings {
		if b.RepositoryID == "" || seen[b.RepositoryID] || (b.RepositoryID != pull && b.RepositoryID != push) {
			return ErrAssignmentInvalid
		}
		seen[b.RepositoryID] = true
		if _, ok := ConfigRepositoryTransport(b.URL); !ok {
			return ErrAssignmentInvalid
		}
	}
	encoded, _ := json.Marshal(struct {
		Rules    []ConfigPathRule
		Bindings []ConfigRepositoryBinding
	}{rules, bindings})
	if len(encoded) > 65536 {
		return ErrAssignmentInvalid
	}
	return nil
}
func ConfigAssignmentRules(raw []byte) []ConfigPathRule {
	out := make([]ConfigPathRule, 0)
	_ = json.Unmarshal(raw, &out)
	return out
}
func ConfigAssignmentBindings(raw []byte) []ConfigRepositoryBinding {
	out := make([]ConfigRepositoryBinding, 0)
	_ = json.Unmarshal(raw, &out)
	return out
}
func validConfigBranch(s string) bool {
	if !safeCoordinateText(s) || len(s) > 255 || strings.HasPrefix(s, "-") || strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".") || strings.Contains(s, "..") || strings.Contains(s, "@{") || strings.ContainsAny(s, " ~^:?*[\\") || s == "@" {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

func configCoordinatesOverlap(a, b string) bool {
	return a == b || a == "." || b == "." || strings.HasPrefix(a, strings.TrimRight(b, "/")+"/") || strings.HasPrefix(b, strings.TrimRight(a, "/")+"/")
}
func configDestinationCoordinate(s string) string {
	normalized := strings.ReplaceAll(s, "\\", "/")
	if strings.Contains(s, "\\") || (len(s) > 1 && s[1] == ':') || strings.HasPrefix(s, "%") {
		normalized = strings.ToLower(normalized)
	}
	return strings.TrimRight(normalized, "/")
}

func validConfigurationRevision(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// Cross-source nested ownership is safe only for the same repository-to-local
// suffix mapping. Unrelated destination aliases cannot gain precedence.
func configNaturalNestedOverride(a, b ConfigPathRule) bool {
	if a.Source == b.Source {
		return false
	}
	matches := func(parent, child ConfigPathRule) bool {
		if parent.Kind != "directory" || parent.RepositoryPath == child.RepositoryPath {
			return false
		}
		var suffix string
		if parent.RepositoryPath == "." {
			suffix = child.RepositoryPath
		} else if strings.HasPrefix(child.RepositoryPath, parent.RepositoryPath+"/") {
			suffix = strings.TrimPrefix(child.RepositoryPath, parent.RepositoryPath+"/")
		} else {
			return false
		}
		expected := strings.TrimRight(parent.LocalPath, "/\\") + "/" + suffix
		return configDestinationCoordinate(expected) == configDestinationCoordinate(child.LocalPath)
	}
	return matches(a, b) || matches(b, a)
}
