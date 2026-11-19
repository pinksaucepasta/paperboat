package peernetwork

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/capver"
)

// walkSigned calls visit for every struct type reachable from the signed configuration.
func walkSigned(t reflect.Type, seen map[reflect.Type]bool, visit func(reflect.Type)) {
	for t.Kind() == reflect.Slice || t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return
	}
	seen[t] = true
	visit(t)
	for i := range t.NumField() {
		walkSigned(t.Field(i).Type, seen, visit)
	}
}

// The release guard: a field added to a signed type without a minimum capability version would reach every released
// client, which strictly decodes the document and rejects it whole (incident 2026-11-19, home_relay).
func TestEverySignedFieldHasAMinimumCapabilityVersion(t *testing.T) {
	fields := map[string]bool{}
	walkSigned(reflect.TypeOf(Configuration{}), map[reflect.Type]bool{}, func(st reflect.Type) {
		for i := range st.NumField() {
			f := st.Field(i)
			key := st.Name() + "." + jsonName(f)
			fields[key] = true
			min, ok := minCapVersion[key]
			switch {
			case jsonName(f) == "":
				t.Errorf("%s.%s has no json name", st.Name(), f.Name)
			case !ok:
				t.Errorf("%s is a signed field with no entry in minCapVersion: add the capver.Version that first understands it (bump shared/capver.Version when it is new)", key)
			case min < 1 || min > capver.Version:
				t.Errorf("%s needs capability version %d, outside 1..%d", key, min, capver.Version)
			case min > 1 && !strings.Contains(f.Tag.Get("json"), "omitempty"):
				t.Errorf("%s is newer than the baseline and must be omitempty, or older clients still see its key", key)
			}
		}
	})
	for key := range minCapVersion {
		if !fields[key] {
			t.Errorf("minCapVersion names %s, which is not a signed field", key)
		}
	}
}

// fullConfiguration has every field of every signed type set, whatever fields exist today.
func fullConfiguration() Configuration {
	var c Configuration
	fill(reflect.ValueOf(&c).Elem())
	return c
}

func fill(v reflect.Value) {
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			fill(v.Field(i))
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fill(v.Index(0))
	case reflect.String:
		v.SetString("x")
	case reflect.Int, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint16, reflect.Uint64:
		v.SetUint(1)
	}
}

// The document built for capability version N holds no field newer than N, as marshalled and as a value.
func TestConfigurationForCapVersionDropsOnlyNewerFields(t *testing.T) {
	full := fullConfiguration()
	for n := 0; n <= capver.Version; n++ {
		got := full.ForCapVersion(n)
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		// What a client of version n knows: the table's fields with a minimum of at most n.
		known := map[string]bool{}
		for key, min := range minCapVersion {
			if min <= max(n, 1) {
				known[key] = true
			}
		}
		if n < 2 && bytes.Contains(raw, []byte("home_relay")) {
			t.Fatalf("capver %d: the signed JSON carries home_relay", n)
		}
		assertOnlyKnown(t, n, reflect.ValueOf(got), known)
		if n < 2 && got.Peers[0].HomeRelay != "" {
			t.Fatalf("capver %d still carries home_relay", n)
		}
		if n >= 2 && got.Peers[0].HomeRelay == "" {
			t.Fatalf("capver %d lost home_relay", n)
		}
	}
	if full.Peers[0].HomeRelay == "" || full.Peers[0].Scopes[0].Port == 0 {
		t.Fatal("ForCapVersion modified the configuration it was called on")
	}
}

// assertOnlyKnown fails for any non-zero field whose key is not in known.
func assertOnlyKnown(t *testing.T, n int, v reflect.Value, known map[string]bool) {
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			key := v.Type().Name() + "." + jsonName(v.Type().Field(i))
			if !v.Field(i).IsZero() && !known[key] {
				t.Errorf("capver %d: document carries %s", n, key)
			}
			assertOnlyKnown(t, n, v.Field(i), known)
		}
	case reflect.Slice:
		for i := range v.Len() {
			assertOnlyKnown(t, n, v.Index(i), known)
		}
	}
}

// A field added after the baseline is dropped for older versions, by the table alone: simulate one.
func TestNewFieldIsOmittedForOlderCapVersions(t *testing.T) {
	type Peer struct { // the name selects the table entries of Peer
		Identity  string `json:"identity"`
		Newer     string `json:"newer,omitempty"`
		Baselined string `json:"endpoints,omitempty"`
	}
	minCapVersion["Peer.newer"] = capver.Version + 1
	defer delete(minCapVersion, "Peer.newer")
	p := Peer{Identity: "i", Newer: "n", Baselined: "e"}
	omitNewer(reflect.ValueOf(&p).Elem(), capver.Version)
	if p.Newer != "" || p.Identity == "" || p.Baselined == "" {
		t.Fatalf("got %+v", p)
	}
	q := Peer{Identity: "i", Newer: "n"}
	omitNewer(reflect.ValueOf(&q).Elem(), capver.Version+1)
	if q.Newer != "n" {
		t.Fatalf("a client that understands the field lost it: %+v", q)
	}
}
