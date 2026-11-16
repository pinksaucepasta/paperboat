package apitokens

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/shared/policy"
)

// ParseScope checks the shape of a token scope: a non-empty list of {dst, allow, ports?}
// entries with non-empty dst and allow, known actions and valid ports. JSON null or an absent
// scope is no scope (nil). Names in dst are resolved against the workspace separately (see
// NameValidator). Error paths are JSON Pointers under /scope, codes are policy codes.
func ParseScope(raw json.RawMessage) ([]policy.ScopeEntry, policy.Errors) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var errs policy.Errors
	add := func(path, code, format string, args ...any) {
		errs = append(errs, policy.Error{Path: path, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		add("/scope", "wrong_type", "scope is a list of {dst, allow, ports} entries")
		return nil, errs
	}
	if len(items) == 0 {
		add("/scope", "empty", "scope needs at least one entry; omit it for no scope")
		return nil, errs
	}
	out := make([]policy.ScopeEntry, 0, len(items))
	for i, item := range items {
		p := "/scope/" + strconv.Itoa(i)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil || fields == nil {
			add(p, "wrong_type", "a scope entry is an object {dst, allow, ports}")
			continue
		}
		var e policy.ScopeEntry
		for key, value := range fields {
			switch key {
			case "dst":
				e.Dst = stringList(add, p+"/dst", value)
			case "allow":
				e.Allow = stringList(add, p+"/allow", value)
			case "ports":
				e.Ports = ports(add, p+"/ports", value)
			default:
				add(p+"/"+key, "unknown_field", "a scope entry has only dst, allow and ports")
			}
		}
		for _, key := range []string{"dst", "allow"} {
			if _, ok := fields[key]; !ok {
				add(p+"/"+key, "required", "%s is required", key)
			}
		}
		for j, a := range e.Allow {
			if a != "*" && !slices.Contains(policy.MachineActions, a) && !slices.Contains(policy.ENVActions, a) {
				add(p+"/allow/"+strconv.Itoa(j), "unknown_action", "unknown action %q; actions are %s, %s", a, strings.Join(policy.MachineActions, ", "), strings.Join(policy.ENVActions, ", "))
			}
		}
		if _, ok := fields["ports"]; ok && !slices.Equal(e.Allow, []string{policy.ActionPrivateAccess}) {
			add(p+"/ports", "ports_need_private_access", "ports apply only when allow is exactly [\"private_access\"]")
		}
		out = append(out, e)
	}
	if len(errs) > 0 {
		return nil, sortErrors(errs)
	}
	return out, nil
}

func stringList(add func(string, string, string, ...any), path string, raw json.RawMessage) []string {
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil || list == nil {
		add(path, "wrong_type", "a list of strings is expected")
		return nil
	}
	if len(list) == 0 {
		add(path, "empty", "the list needs at least one entry")
	}
	return list
}

func ports(add func(string, string, string, ...any), path string, raw json.RawMessage) []policy.PortRange {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		add(path, "wrong_type", "ports is a list of numbers or ranges like \"8000-8100\"")
		return nil
	}
	if len(items) == 0 {
		add(path, "empty", "the list needs at least one entry")
	}
	out := make([]policy.PortRange, 0, len(items))
	for i, item := range items {
		r, ok := portRange(item)
		if !ok || r.From < 1 || r.To > 65535 || r.From > r.To {
			add(path+"/"+strconv.Itoa(i), "invalid_port", "a port is a number from 1 to 65535 or a range like \"8000-8100\"")
			continue
		}
		out = append(out, r)
	}
	return out
}

func portRange(raw json.RawMessage) (policy.PortRange, bool) {
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return policy.PortRange{From: n, To: n}, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return policy.PortRange{}, false
	}
	a, b, found := strings.Cut(s, "-")
	from, err := strconv.Atoi(a)
	if err != nil {
		return policy.PortRange{}, false
	}
	if !found {
		return policy.PortRange{From: from, To: from}, true
	}
	to, err := strconv.Atoi(b)
	return policy.PortRange{From: from, To: to}, err == nil
}

func sortErrors(errs policy.Errors) policy.Errors {
	slices.SortStableFunc(errs, func(a, b policy.Error) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		if c := strings.Compare(a.Code, b.Code); c != 0 {
			return c
		}
		return strings.Compare(a.Message, b.Message)
	})
	return errs
}
