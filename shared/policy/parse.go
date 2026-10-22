package policy

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tailscale/hujson"
)

// Parse reads a policy file: JSON with comments and trailing commas (HuJSON). Unknown keys,
// duplicate keys and wrong types are errors with the path where they occur. Parse checks
// only the shape; Compile checks names against a workspace.
func Parse(src []byte) (Document, Errors) {
	var c collector
	v, err := hujson.Parse(src)
	if err != nil {
		c.add("", "syntax", "%s", syntaxMessage(err))
		return Document{}, c.errs
	}
	var d Document
	obj, ok := object(&c, "", v)
	if !ok {
		return Document{}, c.errs.sorted()
	}
	for _, m := range members(&c, "", obj) {
		p := pointer("", m.name)
		switch m.name {
		case "$schema":
			d.Schema, _ = str(&c, p, m.value)
		case "allow_private_access":
			if b, ok := boolean(&c, p, m.value); ok {
				d.AllowPrivateAccess = &b
			}
		case "groups":
			d.Groups = stringListMap(&c, p, m.value)
		case "tags":
			d.Tags = stringListMap(&c, p, m.value)
		case "rules":
			d.Rules = parseRules(&c, p, m.value)
		case "tests":
			d.Tests = parseTests(&c, p, m.value)
		default:
			c.add(p, "unknown_field", "unknown field %q; allowed: $schema, allow_private_access, groups, tags, rules, tests", m.name)
		}
	}
	return d, c.errs.sorted()
}

// Canonical returns the file as plain, compact JSON (comments and trailing commas removed),
// the form the server stores next to the original text and hashes.
func Canonical(src []byte) ([]byte, error) {
	out, err := hujson.Minimize(src)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := json.Compact(&b, out); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Format pretty-prints a policy file, keeping its comments.
func Format(src []byte) ([]byte, error) { return hujson.Format(src) }

func syntaxMessage(err error) string {
	s := err.Error()
	s = strings.TrimPrefix(s, "hujson: ")
	return "the file is not valid JSON or HuJSON: " + s
}

type member struct {
	name  string
	value hujson.Value
}

func object(c *collector, path string, v hujson.Value) (*hujson.Object, bool) {
	o, ok := v.Value.(*hujson.Object)
	if !ok {
		c.add(path, "wrong_type", "must be an object")
	}
	return o, ok
}

func members(c *collector, path string, o *hujson.Object) []member {
	seen := map[string]bool{}
	out := make([]member, 0, len(o.Members))
	for _, m := range o.Members {
		name := m.Name.Value.(hujson.Literal).String()
		if seen[name] {
			c.add(pointer(path, name), "duplicate_key", "key %q appears more than once", name)
			continue
		}
		seen[name] = true
		out = append(out, member{name: name, value: m.Value})
	}
	return out
}

func array(c *collector, path string, v hujson.Value) ([]hujson.Value, bool) {
	a, ok := v.Value.(*hujson.Array)
	if !ok {
		c.add(path, "wrong_type", "must be a list")
		return nil, false
	}
	return a.Elements, true
}

func str(c *collector, path string, v hujson.Value) (string, bool) {
	l, ok := v.Value.(hujson.Literal)
	if !ok || l.Kind() != '"' {
		c.add(path, "wrong_type", "must be a string")
		return "", false
	}
	return l.String(), true
}

func boolean(c *collector, path string, v hujson.Value) (bool, bool) {
	l, ok := v.Value.(hujson.Literal)
	if !ok || (l.Kind() != 't' && l.Kind() != 'f') {
		c.add(path, "wrong_type", "must be true or false")
		return false, false
	}
	return l.Bool(), true
}

func stringList(c *collector, path string, v hujson.Value) []string {
	items, ok := array(c, path, v)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		if s, ok := str(c, pointer(path, i), item); ok {
			out = append(out, s)
		}
	}
	return out
}

func stringListMap(c *collector, path string, v hujson.Value) map[string][]string {
	o, ok := object(c, path, v)
	if !ok {
		return nil
	}
	out := map[string][]string{}
	for _, m := range members(c, path, o) {
		out[m.name] = stringList(c, pointer(path, m.name), m.value)
	}
	return out
}

func parseRules(c *collector, path string, v hujson.Value) []Rule {
	items, ok := array(c, path, v)
	if !ok {
		return nil
	}
	out := make([]Rule, len(items))
	for i, item := range items {
		p := pointer(path, i)
		o, ok := object(c, p, item)
		if !ok {
			continue
		}
		var r Rule
		have := map[string]bool{}
		for _, m := range members(c, p, o) {
			fp := pointer(p, m.name)
			have[m.name] = true
			switch m.name {
			case "src":
				r.Src = stringList(c, fp, m.value)
			case "dst":
				r.Dst = stringList(c, fp, m.value)
			case "allow":
				r.Allow = stringList(c, fp, m.value)
			case "deny":
				r.Deny = stringList(c, fp, m.value)
			case "ports":
				r.Ports = parsePorts(c, fp, m.value)
			case "note":
				r.Note, _ = str(c, fp, m.value)
			default:
				c.add(fp, "unknown_field", "unknown field %q; allowed: src, dst, allow, deny, ports, note", m.name)
			}
		}
		for _, k := range []string{"src", "dst"} {
			if !have[k] {
				c.add(pointer(p, k), "required", "a rule needs %q", k)
			}
		}
		if have["allow"] == have["deny"] {
			c.add(p, "allow_or_deny", "a rule has exactly one of \"allow\" and \"deny\"")
		}
		out[i] = r
	}
	return out
}

func parseTests(c *collector, path string, v hujson.Value) []Test {
	items, ok := array(c, path, v)
	if !ok {
		return nil
	}
	out := make([]Test, len(items))
	for i, item := range items {
		p := pointer(path, i)
		o, ok := object(c, p, item)
		if !ok {
			continue
		}
		var t Test
		have := map[string]bool{}
		for _, m := range members(c, p, o) {
			fp := pointer(p, m.name)
			have[m.name] = true
			switch m.name {
			case "src":
				t.Src, _ = str(c, fp, m.value)
			case "dst":
				t.Dst, _ = str(c, fp, m.value)
			case "allow":
				t.Allow = stringList(c, fp, m.value)
			case "deny":
				t.Deny = stringList(c, fp, m.value)
			default:
				c.add(fp, "unknown_field", "unknown field %q; allowed: src, dst, allow, deny", m.name)
			}
		}
		for _, k := range []string{"src", "dst"} {
			if !have[k] {
				c.add(pointer(p, k), "required", "a test needs %q", k)
			}
		}
		if !have["allow"] && !have["deny"] {
			c.add(p, "required", "a test needs \"allow\" or \"deny\"")
		}
		out[i] = t
	}
	return out
}

func parsePorts(c *collector, path string, v hujson.Value) []PortRange {
	items, ok := array(c, path, v)
	if !ok {
		return nil
	}
	out := make([]PortRange, 0, len(items))
	for i, item := range items {
		p := pointer(path, i)
		l, ok := item.Value.(hujson.Literal)
		var r PortRange
		switch {
		case ok && l.Kind() == '0':
			n, err := strconv.Atoi(string(l))
			if err != nil {
				ok = false
				break
			}
			r = PortRange{From: n, To: n}
		case ok && l.Kind() == '"':
			r, ok = parsePortRange(l.String())
		default:
			ok = false
		}
		if !ok || r.From < 1 || r.To > 65535 || r.From > r.To {
			c.add(p, "invalid_port", "a port is a number from 1 to 65535 or a range like \"8000-8100\"")
			continue
		}
		out = append(out, r)
	}
	return out
}

func parsePortRange(s string) (PortRange, bool) {
	a, b, found := strings.Cut(s, "-")
	from, err := strconv.Atoi(a)
	if err != nil {
		return PortRange{}, false
	}
	if !found {
		return PortRange{From: from, To: from}, true
	}
	to, err := strconv.Atoi(b)
	return PortRange{From: from, To: to}, err == nil
}
