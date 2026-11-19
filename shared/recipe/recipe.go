// Package recipe is the recipe file (PLAN F26): its schema, validation with file:line:col errors
// and did-you-mean suggestions, the canonical JSON the server stores, and the recipe token. The
// CLI, the server and the console (through the server) use this one package.
package recipe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/policy"
	"github.com/tailscale/hujson"
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

// Recipe is the canonical form: defaults filled, durations as written ("30m", "7d"). The tags are the
// recipe file's schema (see schema.go).
type Recipe struct {
	Schema             string              `json:"$schema,omitempty" doc:"The JSON Schema of this file, so editors check and complete it. Ignored by Paperboat." examples:"[\"https://example.test/schemas/recipe.json\"]"`
	Tags               []string            `json:"tags,omitempty" pattern:"^[a-z0-9][a-z0-9._-]{0,62}$" maxItems:"32" doc:"Tags every machine paired from this recipe gets. A [[reach]] or policy rule can name them as tag:<name>." examples:"[[\"ci\"], [\"homelab\", \"vm\"]]"`
	Reach              []policy.ScopeEntry `json:"reach,omitempty" maxItems:"64" doc:"What a machine paired from this recipe may reach with its own pb, besides itself. A tag no machine carries yet is accepted and matches once one does." examples:"[[{\"dst\": [\"tag:ci\"], \"allow\": [\"exec\", \"files\"]}]]"`
	Expires            string              `json:"expires" ref:"durationExpires" default:"\"90d\"" doc:"How long the recipe's token pairs new machines, from creation (or from a change of this value). Rotate the recipe to extend it." examples:"[\"30d\", \"365d\"]"`
	MaxMachines        int                 `json:"max_machines,omitempty" minimum:"1" maximum:"2147483647" doc:"The most machines that may be paired from this recipe at once. A machine leaves the count when it is removed. Default: no limit beyond the plan." examples:"[1, 20]"`
	RemoveAfterOffline string              `json:"remove_after_offline,omitempty" ref:"durationOffline" doc:"Remove a machine paired from this recipe once it has been offline this long. Default: never." examples:"[\"30m\", \"7d\"]"`
	MachineLifetime    string              `json:"machine_lifetime,omitempty" ref:"durationLifetime" doc:"Remove a machine this long after it paired, whether or not it is online. Default: never." examples:"[\"8h\", \"7d\"]"`
	Env                *Env                `json:"env,omitempty" doc:"The ENV vaults machines paired from this recipe read." examples:"[{\"vaults\": [\"canary-env\"]}]"`
	Config             *Config             `json:"config,omitempty" doc:"The config sync repository these machines pull. A machine with this section only pulls, never pushes." examples:"[{\"repository\": \"dotfiles\"}]"`
	Preview            []Preview           `json:"preview,omitempty" maxItems:"16" doc:"Previews opened on each machine when it pairs." examples:"[[{\"port\": 3000, \"ttl\": \"2h\"}]]"`
	Tunnel             []Tunnel            `json:"tunnel,omitempty" maxItems:"16" doc:"Tunnels opened on each machine when it pairs." examples:"[[{\"port\": 8123, \"private\": true}]]"`
}

type Env struct {
	Vaults []string `json:"vaults" required:"true" pattern:"^[a-z0-9][a-z0-9-]{0,62}$" maxItems:"16" doc:"Names of ENV vaults of this workspace. The recipe can read them; the vault keys are wrapped to the recipe's token on your computer." examples:"[[\"canary-env\"], [\"ci\", \"staging\"]]"`
	Shell  bool     `json:"shell" default:"true" doc:"Whether the vault values are also set in terminals and commands on the machine." examples:"[false]"`
}

type Config struct {
	Repository string `json:"repository" required:"true" minLength:"1" maxLength:"253" doc:"The name of a config sync repository of this workspace." examples:"[\"dotfiles\"]"`
}

type Preview struct {
	Port       int    `json:"port" required:"true" minimum:"1" maximum:"65535" doc:"The local port to open a preview for." examples:"[3000, 8080]"`
	TTL        string `json:"ttl" ref:"durationTTL" default:"\"1h\"" doc:"How long the preview stays open." examples:"[\"30m\", \"2h\"]"`
	Private    bool   `json:"private,omitempty" doc:"Only members signed in to Paperboat can open the preview." examples:"[true]"`
	Team       bool   `json:"team,omitempty" doc:"Only members of the workspace's team can open the preview." examples:"[true]"`
	HostHeader string `json:"host_header,omitempty" minLength:"1" maxLength:"253" doc:"The Host header sent to the local server, for servers that check it." examples:"[\"localhost:3000\"]"`
	Domain     string `json:"domain,omitempty" minLength:"1" maxLength:"253" doc:"A custom domain of this workspace to serve the preview on." examples:"[\"preview.example.test\"]"`
}

type Tunnel struct {
	Name     string `json:"name,omitempty" pattern:"^[a-z0-9][a-z0-9-]{0,62}$" doc:"The tunnel's name: lowercase letters, digits and -, up to 63 characters." examples:"[\"dashboard\"]"`
	Port     int    `json:"port,omitempty" minimum:"1" maximum:"65535" doc:"The local port to open a tunnel to. A tunnel has port or from, not both." examples:"[8123]"`
	From     string `json:"from,omitempty" minLength:"1" maxLength:"253" doc:"A local address to open a tunnel to, instead of a port, like http://localhost:8123. A tunnel has port or from, not both." examples:"[\"http://localhost:8123\"]"`
	Private  bool   `json:"private,omitempty" doc:"Only members signed in to Paperboat can reach the tunnel." examples:"[true]"`
	Team     bool   `json:"team,omitempty" doc:"Only members of the workspace's team can reach the tunnel." examples:"[true]"`
	Domain   string `json:"domain,omitempty" minLength:"1" maxLength:"253" doc:"A custom domain of this workspace to serve the tunnel on." examples:"[\"tunnel.example.test\"]"`
	Duration string `json:"duration,omitempty" ref:"durationTunnel" doc:"How long the tunnel stays open. Default: until the machine is removed." examples:"[\"2h\", \"7d\"]"`
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

// Extension ends every recipe file name.
const Extension = ".jsonc"

// NameFromFile is the recipe name of a file: its base name without the extension ("ci.jsonc" → "ci").
func NameFromFile(path string) (string, error) {
	base := filepath.Base(path)
	name := strings.TrimSuffix(base, Extension)
	if !strings.HasSuffix(base, Extension) {
		return "", fmt.Errorf("%s: a recipe file ends in %s, like ci.jsonc", base, Extension)
	}
	if !ValidName(name) {
		return "", fmt.Errorf("%s: the recipe name comes from the file name, and %q is not one: use lowercase letters, digits and -, up to 63 characters, like ci.jsonc", base, name)
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

// Parse reads a recipe file (HuJSON: JSON with comments and trailing commas) and returns its canonical
// form. file only names the file in errors. Every problem is reported, with line and column.
func Parse(file string, src []byte) (Recipe, error) {
	file = filepath.Base(file)
	if len(src) > MaxSourceBytes {
		return Recipe{}, Errors{{File: file, Message: fmt.Sprintf("the file is %d bytes; a recipe is at most %d", len(src), MaxSourceBytes)}}
	}
	src = bytes.Clone(src) // hujson.Parse aliases its input and Standardize blanks comments in place
	v, err := hujson.Parse(src)
	if err != nil {
		return Recipe{}, syntaxError(file, err)
	}
	loc := newLocator(src, v)
	v.Standardize()
	dec := json.NewDecoder(bytes.NewReader(v.Pack()))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil || raw == nil {
		return Recipe{}, Errors{{File: file, Line: 1, Col: 1, Message: "a recipe is a JSON object: { ... }"}}
	}
	return validate(file, raw, loc)
}

var syntaxPosition = regexp.MustCompile(`^hujson: line (\d+), column (\d+): `)

func syntaxError(file string, err error) error {
	e := &Error{File: file, Message: strings.TrimPrefix(err.Error(), "hujson: ")}
	if m := syntaxPosition.FindStringSubmatch(err.Error()); m != nil {
		e.Line, _ = strconv.Atoi(m[1])
		e.Col, _ = strconv.Atoi(m[2])
		e.Message = strings.TrimPrefix(err.Error(), m[0])
	}
	return Errors{e}
}
