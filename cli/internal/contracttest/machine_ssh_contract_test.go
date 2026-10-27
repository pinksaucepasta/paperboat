package contracttest

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// Every field the client reads from a listed machine's ssh object is one the server documents for it, so a
// renamed server field (host_key_conflict tells "refused as another machine's keys" from "not yet published")
// fails here instead of silently showing the wrong SSH health.
func TestMachineSSHFieldsMatchServerSchema(t *testing.T) {
	raw, err := os.ReadFile("../../../server/docs/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]schema `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	machine, ok := doc.Components.Schemas["Machine"]
	if !ok {
		t.Fatal("server schema has no Machine")
	}
	assertFieldsDocumented(t, "ssh", reflect.TypeFor[api.MachineSSH](), machine.Properties["ssh"])
}

type schema struct {
	Properties map[string]schema `json:"properties"`
}

func assertFieldsDocumented(t *testing.T, path string, typ reflect.Type, server schema) {
	t.Helper()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	for field := range typ.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		documented, ok := server.Properties[name]
		if !ok {
			t.Errorf("client reads %s.%s, which the server does not document", path, name)
			continue
		}
		if inner := field.Type; inner.Kind() == reflect.Pointer && inner.Elem().Kind() == reflect.Struct {
			assertFieldsDocumented(t, path+"."+name, inner, documented)
		}
	}
}
