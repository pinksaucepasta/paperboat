// Package policy is the workspace access policy (PLAN F18): the file format, its validation
// with stable path+code errors, compilation against a workspace inventory into explicit
// grants, API token scopes, recipe reach, and the explain and test evaluation. The server stores and enforces the compiled
// grants; the CLI uses this package to parse and format files locally. Reference:
// README.md in this directory; JSON Schema: schema.json.
package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Machine actions are exactly the server's machine capabilities plus the preview/tunnel
// inspector actions. ENV actions apply only to the env:team target.
const (
	ActionTerminal      = "terminal"
	ActionExec          = "exec"
	ActionManagedSSH    = "managed_ssh"
	ActionFiles         = "files"
	ActionPreviewManage = "preview_manage"
	ActionTunnelManage  = "tunnel_manage"
	ActionPrivateAccess = "private_access"
	ActionInspect       = "inspect"
	ActionReplay        = "replay"
	ActionENVRead       = "env_read"
	ActionENVWrite      = "env_write"
	// ActionRecord: the subject's Paperboat terminal sessions on the target are recorded (Teams).
	ActionRecord = "record"
	// ActionRecordingView: the subject may replay recordings made on the target (Teams).
	ActionRecordingView = "recording_view"
)

// MachineCapabilities are the actions that are machine capabilities on the server
// (teams.MachineCapabilities); TestMachineCapabilitiesMatchServer keeps them equal.
var MachineCapabilities = []string{ActionTerminal, ActionExec, ActionManagedSSH, ActionFiles, ActionPreviewManage, ActionTunnelManage, ActionPrivateAccess}

// MachineActions is what "*" expands to: every action on a machine target.
var MachineActions = append(slices.Clone(MachineCapabilities), ActionInspect, ActionReplay)

// ResourceActions apply to one preview or tunnel target.
var ResourceActions = []string{ActionInspect, ActionReplay}

// RecordingActions apply to machine targets in a team workspace. "*" does not include them and
// no role or owner guardrail implies them: recording and replay need an explicit rule.
var RecordingActions = []string{ActionRecord, ActionRecordingView}

// ENVActions apply to the env:team target.
var ENVActions = []string{ActionENVRead, ActionENVWrite}

// DefaultPrivatePorts are the ports private_access covers when a rule names no ports: the server's
// automatic private access (80, 443 and 1024 and above; shared/machineservices.AutomaticPort).
var DefaultPrivatePorts = []PortRange{{From: 80, To: 80}, {From: 443, To: 443}, {From: 1024, To: 65535}}

// Document is one parsed policy file. Order of rules carries no meaning; it is kept so that
// errors and explanations can name a rule by its index.
type Document struct {
	Schema             string              `json:"$schema,omitempty" doc:"The JSON Schema of this file, so editors check and complete it. Ignored by Paperboat." examples:"[\"https://example.test/schemas/policy.json\"]"`
	AllowPrivateAccess *bool               `json:"allow_private_access,omitempty" doc:"Team workspaces only. When false (the default), no rule gives private_access. In a personal workspace private access is always on and this key is an error." default:"false" examples:"[true]"`
	Groups             map[string][]string `json:"groups,omitempty" ref:"user" keys:"^group:[a-z0-9][a-z0-9._-]{0,62}$" doc:"Named sets of workspace members, used in a rule's src as group:<name>. Each group lists members as user:<email>." examples:"[{\"group:platform\": [\"user:ana@example.test\", \"user:bo@example.test\"]}]"`
	Tags               map[string][]string `json:"tags,omitempty" pattern:"^machine:[^*]+$" keys:"^tag:[a-z0-9][a-z0-9._-]{0,62}$" doc:"Machine tags, used in a rule's dst as tag:<name>. Each tag lists machines as machine:<alias>." examples:"[{\"tag:ci\": [\"machine:ci-1\", \"machine:ci-2\"]}]"`
	Rules              []Rule              `json:"rules,omitempty" doc:"Who may do what on which targets. A deny always wins over an allow, and what no rule allows is denied; the order of rules carries no meaning." examples:"[[{\"src\": [\"group:platform\"], \"dst\": [\"tag:ci\"], \"allow\": [\"terminal\", \"exec\"]}]]"`
	Tests              []Test              `json:"tests,omitempty" doc:"Expectations checked every time the policy is saved; a failing test refuses the save." examples:"[[{\"src\": \"user:ana@example.test\", \"dst\": \"machine:ci-1\", \"allow\": [\"exec\"]}]]"`
	ConfigSync         []ConfigSyncRule    `json:"config_sync,omitempty" doc:"Which config sync repository machines use, chosen by tag. The first matching rule wins; an explicit per-machine choice comes first, and the machine owner still applies it." examples:"[[{\"dst\": [\"tag:ci\"], \"repository\": \"https://git.example.test/team/ci-config.git\", \"branch\": \"main\"}]]"`
}

// ConfigSyncFor returns the first config_sync rule whose targets include machine:* or one of
// the machine's tags, and the target that matched.
func (d Document) ConfigSyncFor(machineTags []string) (ConfigSyncRule, string, bool) {
	for _, r := range d.ConfigSync {
		for _, t := range r.Dst {
			if t == "machine:*" || slices.Contains(machineTags, t) {
				return r, t, true
			}
		}
	}
	return ConfigSyncRule{}, "", false
}

// ConfigSyncRule picks the config sync repository for the machines in Dst (tag:<name> or
// machine:*). The first matching rule wins. It only chooses the repository: the machine owner
// still applies it, and an explicit per-machine choice comes first.
type ConfigSyncRule struct {
	Dst        []string `json:"dst" required:"true" ref:"configSyncTarget" minItems:"1" doc:"The machines this rule covers: machine:* or tag:<name> (a tag defined in tags)." examples:"[[\"tag:ci\"], [\"machine:*\"]]"`
	Repository string   `json:"repository" required:"true" minLength:"1" doc:"The config sync repository: a Git URL (https://, ssh:// or user@host:path) or an absolute path." examples:"[\"https://git.example.test/team/config.git\", \"git@git.example.test:team/config.git\"]"`
	Branch     string   `json:"branch,omitempty" doc:"The Git branch to sync. Default: main." default:"\"main\"" examples:"[\"main\", \"machines\"]"`
	Note       string   `json:"note,omitempty" doc:"A reminder for people reading the file; not used." examples:"[\"CI machines share one config repository\"]"`
}

// Rule allows or denies actions: it has exactly one of Allow and Deny. A deny always wins
// over an allow, so rule order never matters.
type Rule struct {
	Src   []string    `json:"src" required:"true" ref:"subject" minItems:"1" doc:"Who: user:<email>, group:<name>, role:owner, role:admin, role:billing, role:auditor, role:member or * (every member)." examples:"[[\"group:platform\"], [\"user:ana@example.test\", \"role:admin\"]]"`
	Dst   []string    `json:"dst" required:"true" ref:"target" minItems:"1" doc:"What: machine:<alias>, machine:*, tag:<name>, preview:<name>, tunnel:<name> or env:team. One rule's dst is all machines and tags, all previews and tunnels, or env:team." examples:"[[\"tag:ci\"], [\"machine:web\", \"machine:db\"]]"`
	Allow []string    `json:"allow,omitempty" ref:"action" minItems:"1" doc:"The actions this rule gives. Exactly one of allow and deny is set. * means every machine action (not record or recording_view)." examples:"[[\"terminal\", \"exec\"], [\"*\"]]"`
	Deny  []string    `json:"deny,omitempty" ref:"action" minItems:"1" doc:"The actions this rule takes away. A deny wins over every allow. Exactly one of allow and deny is set." examples:"[[\"private_access\"]]"`
	Ports []PortRange `json:"ports,omitempty" minItems:"1" doc:"Only when allow or deny is exactly [\"private_access\"]: the ports it covers. Default: an allow covers 80, 443 and 1024-65535, a deny every port." examples:"[[5432, \"8000-8100\"]]"`
	Note  string      `json:"note,omitempty" doc:"A reminder for people reading the file; not used." examples:"[\"Platform team runs CI\"]"`
}

// Actions returns the rule's action list and whether it is a deny rule.
func (r Rule) Actions() ([]string, bool) {
	if r.Deny != nil {
		return r.Deny, true
	}
	return r.Allow, false
}

type Test struct {
	Src   string   `json:"src" required:"true" ref:"user" doc:"The member to test, as user:<email>." examples:"[\"user:ana@example.test\"]"`
	Dst   string   `json:"dst" required:"true" pattern:"^(machine:[^*]+|preview:.+|tunnel:.+|env:team)$" doc:"One target: machine:<alias>, preview:<name>, tunnel:<name> or env:team." examples:"[\"machine:ci-1\"]"`
	Allow []string `json:"allow,omitempty" doc:"Actions that must be allowed: an action name, or private_access:<port>. A test has allow, deny or both." examples:"[[\"exec\", \"private_access:5432\"]]"`
	Deny  []string `json:"deny,omitempty" doc:"Actions that must be denied: an action name, or private_access:<port>." examples:"[[\"files\"]]"`
}

// PortRange is inclusive. In the file it is a number (5432) or a string ("8000-8100").
type PortRange struct{ From, To int }

func (p PortRange) String() string {
	if p.From == p.To {
		return fmt.Sprint(p.From)
	}
	return fmt.Sprintf("%d-%d", p.From, p.To)
}

func (p PortRange) MarshalJSON() ([]byte, error) {
	if p.From == p.To {
		return []byte(fmt.Sprint(p.From)), nil
	}
	return []byte(`"` + p.String() + `"`), nil
}

// UnmarshalJSON reads a port (5432) or a range ("8000-8100"), as in a policy file.
func (p *PortRange) UnmarshalJSON(b []byte) error {
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		*p = PortRange{From: n, To: n}
	} else {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return errors.New("a port is a number or a range like \"8000-8100\"")
		}
		r, ok := parsePortRange(s)
		if !ok {
			return errors.New("a port is a number or a range like \"8000-8100\"")
		}
		*p = r
	}
	if p.From < 1 || p.To > 65535 || p.From > p.To {
		return errors.New("ports are from 1 to 65535")
	}
	return nil
}

func (p PortRange) contains(port int) bool { return port >= p.From && port <= p.To }

// Error is one validation or test failure. Path is a JSON Pointer into the file ("" is the
// whole file); Code is stable and documented in README.md; Message is for people.
type Error struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Errors is sorted by path, then code, then message, so the same input always gives the
// same list.
type Errors []Error

func (e Errors) Error() string {
	if len(e) == 0 {
		return "policy is valid"
	}
	parts := make([]string, 0, min(len(e), 5))
	for i, x := range e {
		if i == 5 {
			parts = append(parts, fmt.Sprintf("and %d more", len(e)-5))
			break
		}
		parts = append(parts, x.String())
	}
	return strings.Join(parts, "; ")
}

func (e Error) String() string {
	where := e.Path
	if where == "" {
		where = "/"
	}
	return where + ": " + e.Code + ": " + e.Message
}

func (e Errors) sorted() Errors {
	sort.SliceStable(e, func(i, j int) bool {
		if e[i].Path != e[j].Path {
			return e[i].Path < e[j].Path
		}
		if e[i].Code != e[j].Code {
			return e[i].Code < e[j].Code
		}
		return e[i].Message < e[j].Message
	})
	return e
}

type collector struct{ errs Errors }

func (c *collector) add(path, code, format string, args ...any) {
	c.errs = append(c.errs, Error{Path: path, Code: code, Message: fmt.Sprintf(format, args...)})
}

func pointer(base string, parts ...any) string {
	var b strings.Builder
	b.WriteString(base)
	for _, p := range parts {
		b.WriteByte('/')
		s := fmt.Sprint(p)
		s = strings.ReplaceAll(s, "~", "~0")
		b.WriteString(strings.ReplaceAll(s, "/", "~1"))
	}
	return b.String()
}
