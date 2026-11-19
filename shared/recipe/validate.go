package recipe

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/policy"
)

var (
	topKeys     = []string{"$schema", "tags", "reach", "expires", "max_machines", "remove_after_offline", "machine_lifetime", "env", "config", "preview", "tunnel"}
	reachKeys   = []string{"dst", "allow", "ports"}
	envKeys     = []string{"vaults", "shell"}
	configKeys  = []string{"repository"}
	previewKeys = []string{"port", "ttl", "private", "team", "host_header", "domain"}
	tunnelKeys  = []string{"name", "port", "from", "private", "team", "domain", "duration"}
)

type checker struct {
	file string
	loc  *locator
	errs Errors
}

// at records an error at path; onValue points at the value instead of the key.
func (c *checker) at(path []string, onValue bool, format string, args ...any) {
	e := &Error{File: c.file, Path: "/" + strings.Join(path, "/"), Message: fmt.Sprintf(format, args...)}
	line, keyCol, valueCol := c.loc.key(path)
	e.Line, e.Col = line, keyCol
	if onValue && valueCol > 0 {
		e.Col = valueCol
	}
	c.errs = append(c.errs, e)
}

func validate(file string, raw map[string]any, loc *locator) (Recipe, error) {
	c := &checker{file: file, loc: loc}
	r := Recipe{Expires: "90d"}
	for _, d := range loc.duplicate {
		line, col := loc.lineCol(d.offset)
		c.errs = append(c.errs, &Error{File: file, Line: line, Col: col, Path: "/" + strings.Join(d.path, "/"), Message: fmt.Sprintf("%q appears more than once.", d.path[len(d.path)-1])})
	}
	c.unknown(nil, raw, topKeys)
	if v, ok := raw["$schema"]; ok {
		if _, isString := v.(string); !isString {
			c.at([]string{"$schema"}, true, `"$schema" must be a string, like "$schema": "https://example.test/schemas/recipe.json".`)
		}
	}
	r.Tags = c.names(raw, nil, "tags", MaxTags, func(s string) bool { return policy.ValidName(s) }, "a tag name is lowercase letters, digits, '.', '_' or '-', like \"tags\": [\"ci\"]")
	if v, ok := c.duration(raw, nil, "expires", `"90d"`, time.Minute, MaxExpires); ok {
		r.Expires = v
	}
	if d, ok := ParseDuration(r.Expires); ok && d > MaxExpires {
		c.at([]string{"expires"}, false, "expires must be at most 365d. Rotate the recipe to extend it.")
	}
	if v, ok := raw["max_machines"]; ok {
		n, isInt := integer(v)
		if !isInt || n < 1 || n > math.MaxInt32 {
			c.at([]string{"max_machines"}, true, "max_machines must be a whole number, like \"max_machines\": 20.")
		}
		r.MaxMachines = int(n)
	}
	r.RemoveAfterOffline, _ = c.duration(raw, nil, "remove_after_offline", `"30m", "12h" or "7d" (5m to 30d)`, MinOffline, MaxOffline)
	r.MachineLifetime, _ = c.duration(raw, nil, "machine_lifetime", `"8h" or "7d" (5m to 365d)`, MinLifetime, MaxLifetime)
	for i, t := range c.objects(raw, "reach", MaxReach) {
		r.Reach = append(r.Reach, c.reach([]string{"reach", strconv.Itoa(i)}, t))
	}
	if t, ok := c.table(raw, "env"); ok {
		c.unknown([]string{"env"}, t, envKeys)
		env := &Env{Shell: true, Vaults: c.names(t, []string{"env"}, "vaults", MaxVaults, ValidName, "a vault name is lowercase letters, digits and -, like \"vaults\": [\"canary-env\"]")}
		if _, ok := t["vaults"]; !ok {
			c.at([]string{"env"}, false, "\"env\" needs \"vaults\", like \"vaults\": [\"canary-env\"]")
		}
		if v, ok := t["shell"]; ok {
			b, isBool := v.(bool)
			if !isBool {
				c.at([]string{"env", "shell"}, true, "shell must be true or false.")
			}
			env.Shell = b
		}
		r.Env = env
	}
	if t, ok := c.table(raw, "config"); ok {
		c.unknown([]string{"config"}, t, configKeys)
		repo := c.str(t, []string{"config"}, "repository")
		if repo == "" {
			c.at([]string{"config"}, false, "\"config\" needs \"repository\", like \"repository\": \"dotfiles\"")
		}
		r.Config = &Config{Repository: repo}
	}
	for i, t := range c.objects(raw, "preview", MaxPreviews) {
		r.Preview = append(r.Preview, c.preview([]string{"preview", strconv.Itoa(i)}, t))
	}
	for i, t := range c.objects(raw, "tunnel", MaxTunnels) {
		r.Tunnel = append(r.Tunnel, c.tunnel([]string{"tunnel", strconv.Itoa(i)}, t))
	}
	if len(c.errs) > 0 {
		sort.SliceStable(c.errs, func(i, j int) bool { return c.errs[i].Line < c.errs[j].Line })
		return Recipe{}, c.errs
	}
	return r, nil
}

func (c *checker) reach(path []string, t map[string]any) policy.ScopeEntry {
	c.unknown(path, t, reachKeys)
	var e policy.ScopeEntry
	for _, key := range []string{"dst", "allow"} {
		list, ok := c.strings(t, path, key)
		if !ok {
			continue
		}
		if len(list) == 0 {
			c.at(append(path, key), true, "%s needs at least one entry.", key)
		}
		if key == "dst" {
			e.Dst = list
			continue
		}
		e.Allow = list
		actions := append(slices.Clone(policy.MachineActions), policy.ENVActions...)
		for _, a := range list {
			if a != "*" && !slices.Contains(actions, a) {
				hint := ""
				if s := Suggest(a, actions); s != "" {
					hint = fmt.Sprintf(" Did you mean %q?", s)
				}
				c.at(append(path, key), true, "allow has unknown action %q.%s Actions: pb policy schema", a, hint)
			}
		}
	}
	for _, key := range []string{"dst", "allow"} {
		if _, ok := t[key]; !ok {
			c.at(path, false, "a reach entry needs %q, like %s", key, map[string]string{"dst": `"dst": ["tag:ci"]`, "allow": `"allow": ["exec"]`}[key])
		}
	}
	if v, ok := t["ports"]; ok {
		raw, _ := json.Marshal(v)
		if err := json.Unmarshal(raw, &e.Ports); err != nil || len(e.Ports) == 0 || !validPorts(e.Ports) {
			c.at(append(path, "ports"), true, `ports is a list of ports or ranges, like "ports": [5432, "8000-8100"].`)
		} else if !slices.Equal(e.Allow, []string{policy.ActionPrivateAccess}) {
			c.at(append(path, "ports"), false, `ports apply only when allow is exactly ["private_access"].`)
		}
	}
	return e
}

func validPorts(ports []policy.PortRange) bool {
	for _, p := range ports {
		if p.From < 1 || p.To > 65535 || p.From > p.To {
			return false
		}
	}
	return true
}

func (c *checker) preview(path []string, t map[string]any) Preview {
	c.unknown(path, t, previewKeys)
	p := Preview{TTL: "1h"}
	p.Port = c.port(t, path, "port")
	if _, ok := t["port"]; !ok {
		c.at(path, false, "a preview needs \"port\", like \"port\": 3000.")
	}
	if v, ok := c.duration(t, path, "ttl", `"30m" or "2h" (1m to 24h)`, time.Minute, MaxTTL); ok {
		p.TTL = v
	}
	p.Private, p.Team = c.boolean(t, path, "private"), c.boolean(t, path, "team")
	p.HostHeader, p.Domain = c.str(t, path, "host_header"), c.str(t, path, "domain")
	return p
}

func (c *checker) tunnel(path []string, t map[string]any) Tunnel {
	c.unknown(path, t, tunnelKeys)
	var x Tunnel
	x.Name = c.str(t, path, "name")
	if x.Name != "" && !ValidName(x.Name) {
		c.at(append(path, "name"), true, "a tunnel name is lowercase letters, digits and -, up to 63 characters.")
	}
	_, hasPort := t["port"]
	_, hasFrom := t["from"]
	if hasPort == hasFrom {
		c.at(path, false, "a tunnel needs \"port\" or \"from\", not both.")
	}
	x.Port, x.From = c.port(t, path, "port"), c.str(t, path, "from")
	x.Private, x.Team = c.boolean(t, path, "private"), c.boolean(t, path, "team")
	x.Domain = c.str(t, path, "domain")
	x.Duration, _ = c.duration(t, path, "duration", `"2h" or "7d" (1m to 365d)`, time.Minute, MaxLifetime)
	return x
}

// unknown reports keys outside known, suggesting the closest one.
func (c *checker) unknown(path []string, t map[string]any, known []string) {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if slices.Contains(known, k) {
			continue
		}
		hint := ""
		if s := Suggest(k, known); s != "" {
			hint = fmt.Sprintf(" Did you mean %q?", s)
		}
		c.at(append(slices.Clone(path), k), false, "unknown key %q.%s", k, hint)
	}
}

func (c *checker) table(raw map[string]any, key string) (map[string]any, bool) {
	v, ok := raw[key]
	if !ok {
		return nil, false
	}
	t, isTable := v.(map[string]any)
	if !isTable {
		c.at([]string{key}, true, "%s must be an object: write {...} after \"%s\":.", key, key)
		return nil, false
	}
	return t, true
}

// objects reads a list of objects, at most max.
func (c *checker) objects(raw map[string]any, key string, max int) []map[string]any {
	v, ok := raw[key]
	if !ok {
		return nil
	}
	list, isList := v.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		t, isObject := item.(map[string]any)
		if !isObject {
			isList = false
			break
		}
		out = append(out, t)
	}
	if !isList {
		c.at([]string{key}, true, "%s must be a list of objects: write [{...}, {...}].", key)
		return nil
	}
	if len(out) > max {
		c.at([]string{key}, true, "a recipe has at most %d %s entries.", max, key)
		return nil
	}
	return out
}

func (c *checker) strings(t map[string]any, path []string, key string) ([]string, bool) {
	v, ok := t[key]
	if !ok {
		return nil, false
	}
	list, isList := v.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, isString := item.(string)
		if !isString {
			isList = false
			break
		}
		out = append(out, s)
	}
	if !isList {
		c.at(append(slices.Clone(path), key), true, "%s must be a list of names, like \"%s\": [\"a\", \"b\"].", key, key)
		return nil, false
	}
	return out, true
}

func (c *checker) names(t map[string]any, path []string, key string, max int, valid func(string) bool, rule string) []string {
	list, ok := c.strings(t, path, key)
	if !ok {
		return nil
	}
	if len(list) > max {
		c.at(append(slices.Clone(path), key), true, "%s has %d entries; the limit is %d.", key, len(list), max)
	}
	for _, s := range list {
		if !valid(s) {
			c.at(append(slices.Clone(path), key), true, "%q is not valid: %s.", s, rule)
		}
	}
	slices.Sort(list)
	return slices.Compact(list)
}

func (c *checker) str(t map[string]any, path []string, key string) string {
	v, ok := t[key]
	if !ok {
		return ""
	}
	s, isString := v.(string)
	if !isString || s == "" || len(s) > 253 {
		c.at(append(slices.Clone(path), key), true, "%s must be a non-empty string in quotes.", key)
	}
	return s
}

func (c *checker) boolean(t map[string]any, path []string, key string) bool {
	v, ok := t[key]
	if !ok {
		return false
	}
	b, isBool := v.(bool)
	if !isBool {
		c.at(append(slices.Clone(path), key), true, "%s must be true or false.", key)
	}
	return b
}

func (c *checker) port(t map[string]any, path []string, key string) int {
	v, ok := t[key]
	if !ok {
		return 0
	}
	n, isInt := integer(v)
	if !isInt || n < 1 || n > 65535 {
		c.at(append(slices.Clone(path), key), true, "%s must be a port number from 1 to 65535, like \"%s\": 3000.", key, key)
	}
	return int(n)
}

// duration checks an optional duration in [min, max]; ok is false when it is absent or invalid.
func (c *checker) duration(t map[string]any, path []string, key, example string, min, max time.Duration) (string, bool) {
	v, present := t[key]
	if !present {
		return "", false
	}
	s, _ := v.(string)
	d, ok := ParseDuration(s)
	if key == "expires" && ok && d > max {
		return s, true // reported with its own text by the caller
	}
	if !ok || d < min || d > max {
		c.at(append(slices.Clone(path), key), true, "%s must be a duration like %s.", key, example)
		return "", false
	}
	return s, true
}

func integer(v any) (int64, bool) {
	n, isNumber := v.(json.Number)
	if !isNumber {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
}

// Suggest returns the candidate closest to s, or "" when none is close: a small edit distance,
// or one name containing the other ("ssh" → "managed_ssh").
func Suggest(s string, candidates []string) string {
	best, bestDistance := "", math.MaxInt
	for _, c := range candidates {
		d := distance(s, c)
		if len(s) >= 3 && (strings.Contains(c, s) || strings.Contains(s, c)) {
			d = min(d, 1)
		}
		if d < bestDistance {
			best, bestDistance = c, d
		}
	}
	if bestDistance <= max(2, len(s)/3) {
		return best
	}
	return ""
}

func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
