package httpapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	docsEndpoint  = regexp.MustCompile("(GET|POST|PUT|PATCH|DELETE) (/[A-Za-z0-9_/{}.<>:-]+)")
	docsParameter = regexp.MustCompile(`\{[^}]*\}|<[^>]*>|:[a-z_]+`)
)

// The operator and client guide must not describe an endpoint the API does not have.
func TestAPIGuideEndpointsExistInOpenAPI(t *testing.T) {
	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "api.md"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	operations := map[string]bool{}
	for path, methods := range spec.Paths {
		for method := range methods {
			operations[strings.ToUpper(method)+" "+docsParameter.ReplaceAllString(path, "{}")] = true
		}
	}
	matches := docsEndpoint.FindAllStringSubmatch(string(guide), -1)
	if len(matches) < 20 {
		t.Fatalf("found only %d endpoints in docs/api.md; the guide moved or changed format", len(matches))
	}
	for _, match := range matches {
		key := match[1] + " " + strings.TrimRight(docsParameter.ReplaceAllString(match[2], "{}"), "/")
		if !operations[key] {
			t.Errorf("docs/api.md documents %s %s, which docs/openapi.json does not define", match[1], match[2])
		}
	}
}
