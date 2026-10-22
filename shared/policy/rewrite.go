package policy

import "github.com/tailscale/hujson"

// Rewrite keeps a stored policy's names correct when the workspace changes: every string
// value equal to a key of rename ("machine:db2") becomes its value ("machine:db3"), and every
// list entry in remove ("machine:old", "user:gone@example.test") is deleted. A rule left with
// no src or dst, and a test whose src or dst was removed, is deleted too, so a name that comes
// back later never inherits access. Comments and layout are kept. changed is false when
// nothing matched; src must already be a valid policy file.
func Rewrite(src []byte, rename map[string]string, remove map[string]bool) (out []byte, changed bool, err error) {
	v, err := hujson.Parse(src)
	if err != nil {
		return nil, false, err
	}
	r := rewriter{rename: rename, remove: remove}
	r.value(&v)
	root, ok := v.Value.(*hujson.Object)
	if ok {
		for i := range root.Members {
			m := &root.Members[i]
			switch literal(m.Name) {
			case "rules":
				r.dropIncomplete(&m.Value, []string{"src", "dst"}, false)
			case "tests":
				r.dropIncomplete(&m.Value, []string{"src", "dst"}, true)
			}
		}
	}
	if !r.changed {
		return src, false, nil
	}
	return v.Pack(), true, nil
}

type rewriter struct {
	rename  map[string]string
	remove  map[string]bool
	changed bool
}

func literal(v hujson.Value) string {
	if l, ok := v.Value.(hujson.Literal); ok && l.Kind() == '"' {
		return l.String()
	}
	return ""
}

func (r *rewriter) value(v *hujson.Value) {
	switch x := v.Value.(type) {
	case hujson.Literal:
		if x.Kind() == '"' {
			if to, ok := r.rename[x.String()]; ok {
				v.Value = hujson.String(to)
				r.changed = true
			}
		}
	case *hujson.Object:
		for i := range x.Members {
			r.value(&x.Members[i].Value)
		}
	case *hujson.Array:
		kept := x.Elements[:0]
		for i := range x.Elements {
			e := x.Elements[i]
			if s := literal(e); s != "" && r.remove[s] {
				r.changed = true
				continue
			}
			r.value(&e)
			kept = append(kept, e)
		}
		x.Elements = kept
	}
}

// dropIncomplete deletes entries of a rules or tests list that lost a required field's
// content: an empty src/dst list, or (single) a test whose src/dst string was removed.
func (r *rewriter) dropIncomplete(list *hujson.Value, fields []string, single bool) {
	a, ok := list.Value.(*hujson.Array)
	if !ok {
		return
	}
	kept := a.Elements[:0]
	for _, e := range a.Elements {
		if r.incomplete(e, fields, single) {
			r.changed = true
			continue
		}
		kept = append(kept, e)
	}
	a.Elements = kept
}

func (r *rewriter) incomplete(e hujson.Value, fields []string, single bool) bool {
	o, ok := e.Value.(*hujson.Object)
	if !ok {
		return false
	}
	for _, m := range o.Members {
		name := literal(m.Name)
		for _, f := range fields {
			if name != f {
				continue
			}
			if single && r.remove[literal(m.Value)] {
				return true
			}
			if l, ok := m.Value.Value.(*hujson.Array); ok && len(l.Elements) == 0 {
				return true
			}
		}
	}
	return false
}
