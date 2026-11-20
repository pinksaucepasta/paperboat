package operatorreporting

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/workers"
)

// The documented `worker` query enum is the registry's name list, so a worker added to or removed from
// workers.progressNames cannot leave the API document (and the console and admin types generated from it) stale.
func TestOpenAPIWorkerEnumMatchesTheProgressRegistry(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	var enums [][]string
	var walk func(node any)
	walk = func(node any) {
		switch value := node.(type) {
		case map[string]any:
			if description, _ := value["description"].(string); value["name"] == "worker" && strings.HasPrefix(description, "Fixed mounted worker name") {
				schema, _ := value["schema"].(map[string]any)
				var names []string
				items, _ := schema["enum"].([]any)
				for _, item := range items {
					name, _ := item.(string)
					names = append(names, name)
				}
				enums = append(enums, names)
			}
			for _, child := range value {
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	walk(spec)
	if len(enums) == 0 {
		t.Fatal("no `worker` query parameter found in openapi.json")
	}
	want := workers.ProgressNames()
	slices.Sort(want)
	for _, got := range enums {
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Fatalf("openapi worker enum differs from the progress registry:\n got  %v\n want %v", got, want)
		}
	}
}
