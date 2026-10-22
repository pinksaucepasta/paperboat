// Package policy is the workspace access policy (PLAN F18): the file format, its validation
// with stable path+code errors, compilation against a workspace inventory into explicit
// grants, API token scopes, and the explain and test evaluation. The server stores and enforces the compiled
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
)

// MachineCapabilities are the actions that are machine capabilities on the server
// (teams.MachineCapabilities); TestMachineCapabilitiesMatchServer keeps them equal.
var MachineCapabilities = []string{ActionTerminal, ActionExec, ActionManagedSSH, ActionFiles, ActionPreviewManage, ActionTunnelManage, ActionPrivateAccess}

// MachineActions is what "*" expands to: every action on a machine target.
var MachineActions = append(slices.Clone(MachineCapabilities), ActionInspect, ActionReplay)

// ResourceActions apply to one preview or tunnel target.
var ResourceActions = []string{ActionInspect, ActionReplay}

// ENVActions apply to the env:team target.
var ENVActions = []string{ActionENVRead, ActionENVWrite}

// DefaultPrivatePorts is the port range private_access covers when a rule names no ports
// (the server's automatic private access is ports 1024 and above).
var DefaultPrivatePorts = PortRange{From: 1024, To: 65535}

// Document is one parsed policy file. Order of rules carries no meaning; it is kept so that
// errors and explanations can name a rule by its index.
type Document struct {
	Schema             string              `json:"$schema,omitempty"`
	AllowPrivateAccess *bool               `json:"allow_private_access,omitempty"`
	Groups             map[string][]string `json:"groups,omitempty"`
	Tags               map[string][]string `json:"tags,omitempty"`
	Rules              []Rule              `json:"rules,omitempty"`
	Tests              []Test              `json:"tests,omitempty"`
}

// Rule allows or denies actions: it has exactly one of Allow and Deny. A deny always wins
// over an allow, so rule order never matters.
type Rule struct {
	Src   []string    `json:"src"`
	Dst   []string    `json:"dst"`
	Allow []string    `json:"allow,omitempty"`
	Deny  []string    `json:"deny,omitempty"`
	Ports []PortRange `json:"ports,omitempty"`
	Note  string      `json:"note,omitempty"`
}

// Actions returns the rule's action list and whether it is a deny rule.
func (r Rule) Actions() ([]string, bool) {
	if r.Deny != nil {
		return r.Deny, true
	}
	return r.Allow, false
}

type Test struct {
	Src   string   `json:"src"`
	Dst   string   `json:"dst"`
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
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
