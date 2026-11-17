// Package recipe is the recipe file (PLAN F26): its schema, validation with file:line:col errors
// and did-you-mean suggestions, the canonical JSON the server stores, and the recipe token. The
// CLI, the server and the console (through the server) use this one package.
package recipe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/pinksaucepasta/paperboat/shared/policy"
)

// Limits of one recipe; the server and the client check the same values.
const (
	MaxSourceBytes = 64 << 10
	MaxTags        = 32
	MaxReach       = 64
	MaxVaults      = 16
	MaxPreviews    = 16
	MaxTunnels     = 16

	DefaultExpires = 90 * 24 * time.Hour
	MaxExpires     = 365 * 24 * time.Hour
	MinOffline     = 5 * time.Minute
	MaxOffline     = 30 * 24 * time.Hour
	MinLifetime    = 5 * time.Minute
	MaxLifetime    = 365 * 24 * time.Hour
	DefaultTTL     = time.Hour
	MaxTTL         = 24 * time.Hour
)

// Recipe is the canonical form: defaults filled, durations as written ("30m", "7d").
type Recipe struct {
	Tags               []string            `json:"tags,omitempty"`
	Reach              []policy.ScopeEntry `json:"reach,omitempty"`
	Expires            string              `json:"expires"`
	MaxMachines        int                 `json:"max_machines,omitempty"`
	RemoveAfterOffline string              `json:"remove_after_offline,omitempty"`
	MachineLifetime    string              `json:"machine_lifetime,omitempty"`
	Env                *Env                `json:"env,omitempty"`
	Config             *Config             `json:"config,omitempty"`
	Preview            []Preview           `json:"preview,omitempty"`
	Tunnel             []Tunnel            `json:"tunnel,omitempty"`
}

type Env struct {
	Vaults []string `json:"vaults"`
	Shell  bool     `json:"shell"`
}

type Config struct {
	Repository string `json:"repository"`
}

type Preview struct {
	Port       int    `json:"port"`
	TTL        string `json:"ttl"`
	Private    bool   `json:"private,omitempty"`
	Team       bool   `json:"team,omitempty"`
	HostHeader string `json:"host_header,omitempty"`
	Domain     string `json:"domain,omitempty"`
}

type Tunnel struct {
	Name     string `json:"name,omitempty"`
	Port     int    `json:"port,omitempty"`
	From     string `json:"from,omitempty"`
	Private  bool   `json:"private,omitempty"`
	Team     bool   `json:"team,omitempty"`
	Domain   string `json:"domain,omitempty"`
	Duration string `json:"duration,omitempty"`
}

// Canonical is the JSON the client sends and the server stores.
func (r Recipe) Canonical() []byte {
	b, _ := json.Marshal(r)
	return b
}

// ExpiresIn returns the parsed duration (0 = not set); OfflineAfter and Lifetime do the same for their fields.
func (r Recipe) ExpiresIn() time.Duration    { d, _ := ParseDuration(r.Expires); return d }
func (r Recipe) OfflineAfter() time.Duration { d, _ := ParseDuration(r.RemoveAfterOffline); return d }
func (r Recipe) Lifetime() time.Duration     { d, _ := ParseDuration(r.MachineLifetime); return d }

// Vaults returns the vault names the recipe reads.
func (r Recipe) Vaults() []string {
	if r.Env == nil {
		return nil
	}
	return r.Env.Vaults
}

var nameExpression = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidName reports whether s is a recipe or vault name: lowercase letters, digits and '-', up to 63.
func ValidName(s string) bool { return nameExpression.MatchString(s) }

// NameFromFile is the recipe name of a file: its base name without the extension ("ci.toml" → "ci").
func NameFromFile(path string) (string, error) {
	base := filepath.Base(path)
	name := strings.TrimSuffix(base, filepath.Ext(base))
	if !ValidName(name) {
		return "", fmt.Errorf("%s: the recipe name comes from the file name, and %q is not one: use lowercase letters, digits and -, up to 63 characters, like ci.toml", base, name)
	}
	return name, nil
}

var durationExpression = regexp.MustCompile(`^([1-9][0-9]{0,5})([mhd])$`)

// ParseDuration reads "30m", "12h" or "7d".
func ParseDuration(s string) (time.Duration, bool) {
	m := durationExpression.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, _ := strconv.Atoi(m[1])
	unit := map[string]time.Duration{"m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
	return time.Duration(n) * unit, true
}

// Error is one problem in a recipe file. Line and Col are 0 when the position is unknown (JSON).
type Error struct {
	File      string
	Line, Col int
	Path      string
	Message   string
}

func (e *Error) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s:%d:%d: %s", e.File, e.Line, e.Col, e.Message)
	}
	if e.Path != "" {
		return fmt.Sprintf("%s: %s: %s", e.File, e.Path, e.Message)
	}
	return e.File + ": " + e.Message
}

// Errors are every problem found, in file order.
type Errors []*Error

func (e Errors) Error() string {
	lines := make([]string, len(e))
	for i, x := range e {
		lines[i] = x.Error()
	}
	return strings.Join(lines, "\n")
}

// Parse reads a recipe file, TOML or JSON by its extension, and returns its canonical form.
func Parse(filename string, src []byte) (Recipe, error) {
	file := filepath.Base(filename)
	if len(src) > MaxSourceBytes {
		return Recipe{}, Errors{{File: file, Message: fmt.Sprintf("the file is %d bytes; a recipe is at most %d", len(src), MaxSourceBytes)}}
	}
	switch strings.ToLower(filepath.Ext(file)) {
	case ".toml":
		var raw map[string]any
		if _, err := toml.NewDecoder(bytes.NewReader(src)).Decode(&raw); err != nil {
			return Recipe{}, tomlError(file, err)
		}
		return validate(file, raw, newTOMLLocator(src))
	case ".json":
		return ParseJSON(file, src)
	default:
		return Recipe{}, Errors{{File: file, Message: "a recipe file ends in .toml or .json"}}
	}
}

// ParseJSON validates recipe JSON (the canonical form the server receives) as strictly as a file.
func ParseJSON(file string, src []byte) (Recipe, error) {
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil || dec.More() {
		return Recipe{}, Errors{{File: file, Message: "the recipe is not a JSON object"}}
	}
	return validate(file, raw, nil)
}

func tomlError(file string, err error) error {
	var pe toml.ParseError
	if errors.As(err, &pe) {
		msg := pe.Message
		if strings.Contains(msg, "expected value") || strings.Contains(msg, "unexpected EOF") {
			msg = `expected a value after "="`
		}
		return Errors{{File: file, Line: pe.Position.Line, Col: pe.Position.Col, Message: msg}}
	}
	return Errors{{File: file, Message: err.Error()}}
}
