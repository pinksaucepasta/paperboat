package policy

import (
	"slices"

	"github.com/tailscale/hujson"
)

// The console's per-machine "Manage access" dialog and `pb team machine-grant` edit the policy
// through these two functions. A "simple rule" is one the dialog owns: exactly one src, exactly
// one dst machine, an allow list, no ports, no note. Every other rule stays as the file has it.

// SimpleRule is one per-machine grant as the simple view shows it.
type SimpleRule struct {
	Subject string   // "role:member" or "user:<email>"
	Machine string   // "machine:<name>"
	Allow   []string // sorted machine capabilities
}

// SimpleRules lists the simple rules of a parsed file, in file order.
func SimpleRules(d Document) []SimpleRule {
	var out []SimpleRule
	for _, r := range d.Rules {
		if s, ok := simple(r); ok {
			out = append(out, s)
		}
	}
	return out
}

func simple(r Rule) (SimpleRule, bool) {
	if len(r.Src) != 1 || len(r.Dst) != 1 || r.Deny != nil || r.Ports != nil || r.Note != "" || len(r.Allow) == 0 {
		return SimpleRule{}, false
	}
	if r.Src[0] != "role:member" && (len(r.Src[0]) < 6 || r.Src[0][:5] != "user:") {
		return SimpleRule{}, false
	}
	if len(r.Dst[0]) < 9 || r.Dst[0][:8] != "machine:" || r.Dst[0] == "machine:*" {
		return SimpleRule{}, false
	}
	for _, a := range r.Allow {
		if !slices.Contains(MachineCapabilities, a) {
			return SimpleRule{}, false
		}
	}
	allow := slices.Clone(r.Allow)
	slices.Sort(allow)
	return SimpleRule{Subject: r.Src[0], Machine: r.Dst[0], Allow: allow}, true
}

// SetSimpleRule replaces the simple rules for (subject, machine) with one rule allowing
// actions, or removes them when actions is empty. Comments and other rules are kept.
func SetSimpleRule(src []byte, subject, machine string, actions []string) ([]byte, error) {
	v, err := hujson.Parse(src)
	if err != nil {
		return nil, err
	}
	root, ok := v.Value.(*hujson.Object)
	if !ok {
		return nil, Errors{{Code: "wrong_type", Message: "must be an object"}}
	}
	rules := objectMember(root, "rules")
	if rules == nil {
		if len(actions) == 0 {
			return src, nil
		}
		root.Members = append(root.Members, hujson.ObjectMember{Name: hujson.Value{Value: hujson.String("rules")}, Value: hujson.Value{Value: &hujson.Array{}}})
		rules = &root.Members[len(root.Members)-1].Value
	}
	arr, ok := rules.Value.(*hujson.Array)
	if !ok {
		return nil, Errors{{Path: "/rules", Code: "wrong_type", Message: "must be a list"}}
	}
	kept := arr.Elements[:0]
	for _, e := range arr.Elements {
		var r Rule
		if b, err := hujson.Standardize(e.Pack()); err == nil {
			if d, errs := Parse([]byte(`{"rules":[` + string(b) + `]}`)); len(errs) == 0 {
				r = d.Rules[0]
			}
		}
		if s, ok := simple(r); ok && s.Subject == subject && s.Machine == machine {
			continue
		}
		kept = append(kept, e)
	}
	arr.Elements = kept
	if len(actions) > 0 {
		sorted := slices.Clone(actions)
		slices.Sort(sorted)
		rule := hujson.Value{Value: &hujson.Object{Members: []hujson.ObjectMember{
			{Name: hujson.Value{Value: hujson.String("src")}, Value: stringArray([]string{subject})},
			{Name: hujson.Value{Value: hujson.String("dst")}, Value: stringArray([]string{machine})},
			{Name: hujson.Value{Value: hujson.String("allow")}, Value: stringArray(slices.Compact(sorted))},
		}}}
		arr.Elements = append(arr.Elements, rule)
	}
	v.Format()
	return v.Pack(), nil
}

// SetAllowPrivateAccess sets the team switch in the file.
func SetAllowPrivateAccess(src []byte, on bool) ([]byte, error) {
	v, err := hujson.Parse(src)
	if err != nil {
		return nil, err
	}
	root, ok := v.Value.(*hujson.Object)
	if !ok {
		return nil, Errors{{Code: "wrong_type", Message: "must be an object"}}
	}
	if m := objectMember(root, "allow_private_access"); m != nil {
		m.Value = hujson.Bool(on)
	} else {
		root.Members = append([]hujson.ObjectMember{{Name: hujson.Value{Value: hujson.String("allow_private_access")}, Value: hujson.Value{Value: hujson.Bool(on)}}}, root.Members...)
	}
	v.Format()
	return v.Pack(), nil
}

func objectMember(o *hujson.Object, name string) *hujson.Value {
	for i := range o.Members {
		if literal(o.Members[i].Name) == name {
			return &o.Members[i].Value
		}
	}
	return nil
}

func stringArray(items []string) hujson.Value {
	a := &hujson.Array{}
	for _, s := range items {
		a.Elements = append(a.Elements, hujson.Value{Value: hujson.String(s)})
	}
	return hujson.Value{Value: a}
}
