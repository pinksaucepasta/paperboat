package httpapi

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestOpenAPIAllReferencesResolve(t *testing.T) {
	raw, err := os.ReadFile("../../docs/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	var visit func(any, string)
	visit = func(value any, path string) {
		switch node := value.(type) {
		case map[string]any:
			if rawRef, exists := node["$ref"]; exists {
				ref, ok := rawRef.(string)
				if !ok || !strings.HasPrefix(ref, "#/") {
					t.Errorf("%s: unsupported reference %v", path, rawRef)
				} else {
					target := document
					for _, token := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
						token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
						switch parent := target.(type) {
						case map[string]any:
							target = parent[token]
						case []any:
							index, err := strconv.Atoi(token)
							if err != nil || index < 0 || index >= len(parent) {
								target = nil
							} else {
								target = parent[index]
							}
						default:
							target = nil
						}
						if target == nil {
							t.Errorf("%s: unresolved reference %s", path, ref)
							break
						}
					}
				}
			}
			for key, child := range node {
				visit(child, path+"/"+key)
			}
		case []any:
			for index, child := range node {
				visit(child, fmt.Sprintf("%s/%d", path, index))
			}
		}
	}
	visit(document, "#")
}
