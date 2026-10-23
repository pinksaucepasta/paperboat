package policy

import (
	"strings"

	"github.com/tailscale/hujson"
)

// RewriteFunc rewrites the references of a policy file: every string in src, dst, tags,
// groups and tests is passed to fn, which returns its replacement and whether to keep it. A
// dropped list entry is deleted; a rule left with no src or dst, and a test whose src or dst
// was dropped, is deleted too, so a name that comes back later never inherits access.
// Comments and layout are kept. changed is false when fn changed nothing.
func RewriteFunc(src []byte, fn func(string) (string, bool)) (out []byte, changed bool, err error) {
	v, err := hujson.Parse(src)
	if err != nil {
		return nil, false, err
	}
	r := rewriter{fn: fn}
	r.value(&v, true)
	if root, ok := v.Value.(*hujson.Object); ok {
		for i := range root.Members {
			m := &root.Members[i]
			switch literal(m.Name) {
			case "rules":
				r.dropIncomplete(&m.Value)
			case "tests":
				r.dropIncomplete(&m.Value)
			}
		}
	}
	if !r.changed {
		return src, false, nil
	}
	return v.Pack(), true, nil
}

// Rewrite renames (rename: old → new) and removes (remove) exact references.
func Rewrite(src []byte, rename map[string]string, remove map[string]bool) ([]byte, bool, error) {
	return RewriteFunc(src, func(s string) (string, bool) {
		if remove[s] {
			return s, false
		}
		if to, ok := rename[s]; ok {
			return to, true
		}
		return s, true
	})
}

type rewriter struct {
	fn      func(string) (string, bool)
	dropped map[*hujson.Object]bool
	changed bool
}

func literal(v hujson.Value) string {
	if l, ok := v.Value.(hujson.Literal); ok && l.Kind() == '"' {
		return l.String()
	}
	return ""
}

func (r *rewriter) apply(v *hujson.Value) bool {
	l, ok := v.Value.(hujson.Literal)
	if !ok || l.Kind() != '"' {
		return true
	}
	to, keep := r.fn(l.String())
	if !keep {
		r.changed = true
		return false
	}
	if to != l.String() {
		v.Value = hujson.String(to)
		r.changed = true
	}
	return true
}

// value walks v; refs is false under keys whose strings are not references (note, $schema,
// allow, deny, ports).
func (r *rewriter) value(v *hujson.Value, refs bool) bool {
	switch x := v.Value.(type) {
	case hujson.Literal:
		if refs {
			return r.apply(v)
		}
	case *hujson.Object:
		for i := range x.Members {
			name := literal(x.Members[i].Name)
			inner := refs && name != "note" && name != "$schema" && name != "allow" && name != "deny" && name != "ports"
			if !r.value(&x.Members[i].Value, inner) {
				if r.dropped == nil {
					r.dropped = map[*hujson.Object]bool{}
				}
				r.dropped[x] = true
			}
		}
	case *hujson.Array:
		var lead hujson.Extra
		if len(x.Elements) > 0 {
			lead = x.Elements[0].BeforeExtra
		}
		kept := x.Elements[:0]
		for i := range x.Elements {
			e := x.Elements[i]
			if r.value(&e, refs) {
				kept = append(kept, e)
			}
		}
		x.Elements = kept
		if len(kept) > 0 {
			kept[0].BeforeExtra = lead
		}
	}
	return true
}

// dropIncomplete deletes rules or tests that lost a src or dst: an emptied list, or a test's
// single src/dst string that was dropped.
func (r *rewriter) dropIncomplete(list *hujson.Value) {
	a, ok := list.Value.(*hujson.Array)
	if !ok {
		return
	}
	kept := a.Elements[:0]
	for _, e := range a.Elements {
		if r.incomplete(e) {
			r.changed = true
			continue
		}
		kept = append(kept, e)
	}
	a.Elements = kept
}

func (r *rewriter) incomplete(e hujson.Value) bool {
	o, ok := e.Value.(*hujson.Object)
	if !ok {
		return false
	}
	if r.dropped[o] {
		return true
	}
	for _, m := range o.Members {
		if name := literal(m.Name); name == "src" || name == "dst" {
			if l, ok := m.Value.Value.(*hujson.Array); ok && len(l.Elements) == 0 {
				return true
			}
		}
	}
	return false
}

// Internalize turns names into stable IDs for storage: machine:<name> → machine:#<id>,
// user:<email> → user:#<account> and preview:/tunnel:<name> → preview:/tunnel:#<id>. Names that do not resolve are left as they are (Validate
// reports them). Stored files survive renames and alias collisions unchanged.
func Internalize(src []byte, inv Inventory) ([]byte, error) {
	x := newIndex(inv)
	out, _, err := RewriteFunc(src, func(s string) (string, bool) {
		if name, ok := strings.CutPrefix(s, "machine:"); ok && name != "*" && !strings.HasPrefix(name, "#") {
			if m, code := x.machine(name); code == "" {
				return "machine:#" + m.ID, true
			}
		}
		if strings.HasPrefix(s, "user:") && !strings.HasPrefix(s, "user:#") {
			if a, ok := x.user(s); ok {
				return "user:#" + a, true
			}
		}
		if r, ok := x.resources[s]; ok {
			return r.Kind + ":#" + r.ID, true
		}
		return s, true
	})
	return out, err
}

// Externalize renders a stored file with current names, dropping references to machines,
// members, previews and tunnels no longer in the workspace.
func Externalize(src []byte, inv Inventory) ([]byte, error) {
	x := newIndex(inv)
	out, _, err := RewriteFunc(src, func(s string) (string, bool) {
		if id, ok := strings.CutPrefix(s, "machine:#"); ok {
			m, ok := x.byMachineID[id]
			return "machine:" + x.machineName(m), ok
		}
		if id, ok := strings.CutPrefix(s, "user:#"); ok {
			m, ok := x.byAccount[id]
			return "user:" + strings.ToLower(m.Email), ok
		}
		if kind, id, ok := strings.Cut(s, ":#"); ok && (kind == "preview" || kind == "tunnel") {
			_, ok := x.resourceIDs[kind+":"+id]
			return x.targetLabel(Target{Kind: kind, ID: id}), ok
		}
		return s, true
	})
	return out, err
}
