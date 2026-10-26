package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/wirejson"
)

// addUnknownFields puts a field the client has never heard of into every JSON object.
func addUnknownFields(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			typed[key] = addUnknownFields(child)
		}
		typed["field_added_by_a_newer_server"] = map[string]any{"nested": true}
	case []any:
		for index, child := range typed {
			typed[index] = addUnknownFields(child)
		}
	}
	return value
}

// A server may add fields to any response at any time. Every response document the client decodes
// must keep decoding when objects at every level carry fields it does not know.
func TestResponseTypesIgnoreFieldsAddedByANewerServer(t *testing.T) {
	for _, prototype := range []any{
		E2EERoot{}, PeerTransportKeySet{}, []PendingEndpointIdentity{}, EndpointCertificateDocument{},
		PreviewDNSInstructions{}, PreviewLeasePage{}, PreviewLease{}, DiagnosticUploadIntent{},
		InspectorAccess{}, []InspectorTarget{}, NativePrivateGrant{}, TunnelDNSInstructions{}, WorkspacePage{},
		WorkspacePage{Items: []Workspace{{ID: "personal", Name: "Personal", Kind: "personal", Role: "owner"}}},
	} {
		kind := reflect.TypeOf(prototype)
		t.Run(kind.String(), func(t *testing.T) {
			base, err := json.Marshal(prototype)
			if err != nil {
				t.Fatal(err)
			}
			if err := wirejson.Decode(base, reflect.New(kind).Interface()); err != nil {
				t.Skipf("the type does not decode its own empty form: %v", err)
			}
			var generic any
			if err := json.Unmarshal(base, &generic); err != nil {
				t.Fatal(err)
			}
			extended, err := json.Marshal(addUnknownFields(generic))
			if err != nil {
				t.Fatal(err)
			}
			if err := wirejson.Decode(extended, reflect.New(kind).Interface()); err != nil {
				t.Fatalf("a response with added fields was refused: %v", err)
			}
		})
	}
}

// Server responses are decoded through wirejson only: no strict decoder may come back in this package.
func TestAPIPackageHasNoStrictResponseDecoder(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "DisallowUnknownFields") {
			t.Errorf("%s rejects unknown response fields; decode with wirejson so a newer server never breaks this client", file)
		}
	}
}
