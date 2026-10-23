package policy

import (
	"slices"

	"github.com/tailscale/hujson"
)

// Machine tags are edited one machine at a time (`pb machine tag`, the console's "Edit tags")
// through these functions; the file's tags map stays the one owner of tags.

// MachineTags lists the tags whose entries name machine ("machine:<name>"), sorted.
func MachineTags(d Document, machine string) []string {
	out := []string{}
	for name, entries := range d.Tags {
		if slices.Contains(entries, machine) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// SetMachineTags makes machine ("machine:<name>") carry exactly tags: it is added to each of them
// (creating a tag that does not exist) and removed from every other. A tag left with no machines
// is removed unless a rule or test names it, so those stay valid. Comments and other entries are
// kept. Tag names are checked by Validate, like any edit.
func SetMachineTags(src []byte, machine string, tags []string) ([]byte, error) {
	v, err := hujson.Parse(src)
	if err != nil {
		return nil, err
	}
	root, ok := v.Value.(*hujson.Object)
	if !ok {
		return nil, Errors{{Code: "wrong_type", Message: "must be an object"}}
	}
	referenced := tagReferences(src)
	member := objectMember(root, "tags")
	if member == nil {
		if len(tags) == 0 {
			return src, nil
		}
		root.Members = append(root.Members, hujson.ObjectMember{Name: hujson.Value{Value: hujson.String("tags")}, Value: hujson.Value{Value: &hujson.Object{}}})
		member = &root.Members[len(root.Members)-1].Value
	}
	obj, ok := member.Value.(*hujson.Object)
	if !ok {
		return nil, Errors{{Path: "/tags", Code: "wrong_type", Message: "must be an object"}}
	}
	want := map[string]bool{}
	for _, t := range tags {
		want[t] = true
	}
	kept := obj.Members[:0]
	for _, m := range obj.Members {
		name := literal(m.Name)
		arr, isArray := m.Value.Value.(*hujson.Array)
		if !isArray {
			kept = append(kept, m)
			continue
		}
		has := false
		elements := arr.Elements[:0]
		for _, e := range arr.Elements {
			if literal(e) == machine {
				if !want[name] || has {
					continue
				}
				has = true
			}
			elements = append(elements, e)
		}
		arr.Elements = elements
		if want[name] && !has {
			arr.Elements = append(arr.Elements, hujson.Value{Value: hujson.String(machine)})
		}
		delete(want, name)
		if len(arr.Elements) == 0 && !referenced[name] {
			continue
		}
		kept = append(kept, m)
	}
	obj.Members = kept
	added := make([]string, 0, len(want))
	for name := range want {
		added = append(added, name)
	}
	slices.Sort(added)
	for _, name := range added {
		obj.Members = append(obj.Members, hujson.ObjectMember{Name: hujson.Value{Value: hujson.String(name)}, Value: stringArray([]string{machine})})
	}
	v.Format()
	return v.Pack(), nil
}

// tagReferences reports the tags rules and tests name.
func tagReferences(src []byte) map[string]bool {
	out := map[string]bool{}
	d, errs := Parse(src)
	if len(errs) > 0 {
		return out
	}
	for _, r := range d.Rules {
		for _, s := range append(slices.Clone(r.Src), r.Dst...) {
			out[s] = true
		}
	}
	for _, t := range d.Tests {
		out[t.Src], out[t.Dst] = true, true
	}
	return out
}
