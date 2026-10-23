package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

type listsProbe struct {
	Items    []string         `json:"items"`
	Nested   *listsProbe      `json:"nested,omitempty"`
	ByKey    map[string][]int `json:"by_key"`
	Raw      json.RawMessage  `json:"raw,omitempty"`
	Bytes    []byte           `json:"bytes"`
	At       time.Time        `json:"at"`
	Children []listsProbe     `json:"children"`
	Any      any              `json:"any"`
	Fields   map[string]any   `json:"fields"`
	hidden   []string
}

// GET /v1/machines?state=revoked answered "items": null (a nil Go slice) and crashed the console's machine page.
// Every list in a response is [], never null, however deep it sits; values with their own JSON encoding and byte
// slices keep theirs.
func TestResponseListsAreNeverNull(t *testing.T) {
	at := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	payload := SuccessResponse{Data: map[string]any{
		"items":      []string(nil),
		"pagination": map[string]any{"total": 0},
		"probe": listsProbe{Nested: &listsProbe{}, ByKey: map[string][]int{"a": nil}, At: at, Children: []listsProbe{{}},
			Any: []int(nil), Fields: map[string]any{"list": []string(nil)}, hidden: nil},
	}}
	recorder := httptest.NewRecorder()
	writeJSON(recorder, 200, payload)
	body := recorder.Body.String()
	for _, want := range []string{`"items":[]`, `"by_key":{"a":[]}`, `"children":[{"items":[]`, `"any":[]`, `"fields":{"list":[]}`,
		`"nested":{"items":[]`, `"at":"2026-10-23T12:00:00Z"`, `"bytes":null`} {
		if !strings.Contains(body, want) {
			t.Fatalf("response %s\nlacks %s", body, want)
		}
	}
	if strings.Contains(body, `"items":null`) {
		t.Fatalf("a nil list was encoded as null: %s", body)
	}
	recorder = httptest.NewRecorder()
	writeJSON(recorder, 204, nil) // a nil payload is still written as it was
	if strings.TrimSpace(recorder.Body.String()) != "null" {
		t.Fatalf("nil payload: %q", recorder.Body.String())
	}
}

// The OpenAPI document never declares a list nullable: clients may rely on [] (the server never sends null).
func TestOpenAPIArraysAreNeverNullable(t *testing.T) {
	raw, err := os.ReadFile("../../docs/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	var walk func(node any, path string)
	walk = func(node any, path string) {
		switch value := node.(type) {
		case map[string]any:
			if types, ok := value["type"].([]any); ok {
				array, null := false, false
				for _, kind := range types {
					array, null = array || kind == "array", null || kind == "null"
				}
				if array && null {
					t.Errorf("%s: a nullable array; lists are always [] (TestResponseListsAreNeverNull)", path)
				}
			}
			for key, child := range value {
				walk(child, path+"/"+key)
			}
		case []any:
			for _, child := range value {
				walk(child, path)
			}
		}
	}
	walk(document, "")
}
