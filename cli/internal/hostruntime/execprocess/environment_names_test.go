package execprocess

import (
	"reflect"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
)

// Names differing only by case are one variable on Windows (the managed value takes the process's own spelling) and
// two variables on Linux and macOS; the delivered values must not be swapped between them.
func TestMergedEnvironmentListsFollowsTheOSNameRule(t *testing.T) {
	prior := envinject.CaseInsensitiveNames
	t.Cleanup(func() { envinject.CaseInsensitiveNames = prior })
	envinject.CaseInsensitiveNames = false
	got := mergedEnvironmentLists([]string{"HOME=/h"}, []string{"XPC_CASE=override", "XPC_Case=personal"})
	if want := []string{"HOME=/h", "XPC_CASE=override", "XPC_Case=personal"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("case-sensitive: %v", got)
	}
	if got := mergedEnvironment([]string{"Path=/b"}, map[string]string{"PATH": "/m"}); !reflect.DeepEqual(got, []string{"PATH=/m", "Path=/b"}) {
		t.Fatalf("case-sensitive map: %v", got)
	}
	envinject.CaseInsensitiveNames = true
	if got := mergedEnvironmentLists([]string{"Path=/b"}, []string{"PATH=/m"}); !reflect.DeepEqual(got, []string{"Path=/m"}) {
		t.Fatalf("case-insensitive: %v", got)
	}
}
