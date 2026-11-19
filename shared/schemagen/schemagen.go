// Package schemagen builds a JSON Schema (2020-12) from the Go types a file is decoded into, so the
// schema cannot drift from the types the CLI and server validate against. Fields carry their schema
// in struct tags; what a tag cannot say is passed in Options. A test in each owning package
// regenerates the schema and fails when the checked-in file differs (go test -update rewrites it).
//
// Tags: doc (required), examples (a JSON array), default (JSON), required:"true", ref (a Defs name
// for the value, or for each item of a list), pattern, keys (pattern of a map's keys), minLength,
// maxLength, minimum, maximum, minItems, maxItems, unique:"true". On a list, pattern, ref and the
// length and range tags apply to its items.
package schemagen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Options is what the types and their tags cannot say.
type Options struct {
	Title       string
	Description string
	Examples    []json.RawMessage
	// Defs are named schemas that fields refer to with ref:"name" (enums, patterns).
	Defs map[string]any
	// Types maps a Go type with its own JSON form to a Defs entry (a port number or range).
	Types map[reflect.Type]string
	// Extra adds keywords to a struct type's schema: description, examples, oneOf, if/then.
	Extra map[reflect.Type]map[string]any
	// Post edits the finished schema (a variant of one type that tags cannot express).
	Post func(schema map[string]any)
}

// Generate returns the schema of root (a struct) as indented JSON with a trailing newline.
func Generate(root any, o Options) ([]byte, error) {
	g := &generator{o: o, defs: map[string]any{}}
	out := map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "title": o.Title, "description": o.Description}
	for k, v := range g.object(reflect.TypeOf(root)) {
		out[k] = v
	}
	if len(o.Examples) > 0 {
		out["examples"] = o.Examples
	}
	for k, v := range o.Defs {
		g.defs[k] = v
	}
	out["$defs"] = g.defs
	if o.Post != nil {
		o.Post(out)
	}
	prune(out)
	if g.err != nil {
		return nil, g.err
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

type generator struct {
	o    Options
	defs map[string]any
	err  error
}

func (g *generator) fail(format string, args ...any) {
	if g.err == nil {
		g.err = fmt.Errorf(format, args...)
	}
}

// object is the schema of a struct type: its properties, required list and Extra keywords.
func (g *generator) object(t reflect.Type) map[string]any {
	props := map[string]any{}
	var required []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" || name == "" {
			continue
		}
		props[name] = g.field(t, f)
		if f.Tag.Get("required") == "true" {
			required = append(required, name)
		}
	}
	s := map[string]any{"type": "object", "additionalProperties": false, "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	for k, v := range g.o.Extra[t] {
		s[k] = v
	}
	return s
}

func (g *generator) field(owner reflect.Type, f reflect.StructField) map[string]any {
	s := map[string]any{}
	t := f.Type
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case g.o.Types[t] == "" && t.Kind() == reflect.Map:
		s["type"] = "object"
		if k := f.Tag.Get("keys"); k != "" {
			s["propertyNames"] = map[string]any{"pattern": k}
		}
		if t.Elem().Kind() == reflect.Slice {
			s["additionalProperties"] = g.list(f, t.Elem().Elem())
		} else {
			s["additionalProperties"] = g.leaf(f, t.Elem())
		}
	case g.o.Types[t] == "" && t.Kind() == reflect.Slice:
		for k, v := range g.list(f, t.Elem()) {
			s[k] = v
		}
	default:
		for k, v := range g.leaf(f, t) {
			s[k] = v
		}
	}
	if d := f.Tag.Get("doc"); d != "" {
		s["description"] = d
	} else {
		g.fail("schemagen: %s.%s has no doc tag", owner.Name(), f.Name)
	}
	if e := f.Tag.Get("examples"); e != "" {
		s["examples"] = g.json(owner, f, e)
	}
	if d := f.Tag.Get("default"); d != "" {
		s["default"] = g.json(owner, f, d)
	}
	return s
}

// list is an array schema whose items follow the field's item tags.
func (g *generator) list(f reflect.StructField, elem reflect.Type) map[string]any {
	s := map[string]any{"type": "array", "items": g.leaf(f, elem)}
	g.number(s, f, "minItems", "minItems")
	g.number(s, f, "maxItems", "maxItems")
	if f.Tag.Get("unique") == "true" {
		s["uniqueItems"] = true
	}
	return s
}

// leaf is the schema of a value: a Defs reference, a struct, or a scalar with its constraints.
func (g *generator) leaf(f reflect.StructField, t reflect.Type) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if name := g.o.Types[t]; name != "" {
		return map[string]any{"$ref": "#/$defs/" + name}
	}
	if ref := f.Tag.Get("ref"); ref != "" {
		return map[string]any{"$ref": "#/$defs/" + ref}
	}
	s := map[string]any{}
	switch t.Kind() {
	case reflect.Struct:
		name := strings.ToLower(t.Name()[:1]) + t.Name()[1:]
		if _, done := g.defs[name]; !done {
			g.defs[name] = nil // the type refers to itself only through this name
			g.defs[name] = g.object(t)
		}
		return map[string]any{"$ref": "#/$defs/" + name}
	case reflect.String:
		s["type"] = "string"
		if p := f.Tag.Get("pattern"); p != "" {
			s["pattern"] = p
		}
		g.number(s, f, "minLength", "minLength")
		g.number(s, f, "maxLength", "maxLength")
	case reflect.Bool:
		s["type"] = "boolean"
	case reflect.Int, reflect.Int64:
		s["type"] = "integer"
		g.number(s, f, "minimum", "minimum")
		g.number(s, f, "maximum", "maximum")
	default:
		g.fail("schemagen: %s has an unsupported type %s", f.Name, t)
	}
	return s
}

func (g *generator) number(s map[string]any, f reflect.StructField, tag, keyword string) {
	v := f.Tag.Get(tag)
	if v == "" {
		return
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		g.fail("schemagen: %s: tag %s is not a number", f.Name, tag)
	}
	s[keyword] = n
}

func (g *generator) json(owner reflect.Type, f reflect.StructField, v string) any {
	var out any
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		g.fail("schemagen: %s.%s: %q is not JSON", owner.Name(), f.Name, v)
	}
	return out
}

// prune drops the Defs nothing refers to, so a package can share a set of Defs.
func prune(schema map[string]any) {
	defs, _ := schema["$defs"].(map[string]any)
	used := map[string]bool{}
	var visit func(v any)
	visit = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if ref, ok := x["$ref"].(string); ok && !used[ref] {
				used[ref] = true
				visit(defs[strings.TrimPrefix(ref, "#/$defs/")])
			}
			for k, child := range x {
				if k != "$defs" {
					visit(child)
				}
			}
		case []any:
			for _, child := range x {
				visit(child)
			}
		case []map[string]any:
			for _, child := range x {
				visit(child)
			}
		}
	}
	visit(schema)
	for name := range defs {
		if !used["#/$defs/"+name] {
			delete(defs, name)
		}
	}
}
