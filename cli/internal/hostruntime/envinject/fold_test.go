package envinject

import (
	"reflect"
	"sort"
	"testing"
)

func overlaid(t *testing.T, insensitive bool) ([]string, map[string]string) {
	t.Helper()
	prior := CaseInsensitiveNames
	CaseInsensitiveNames = insensitive
	t.Cleanup(func() { CaseInsensitiveNames = prior })
	values, byFold, scopes := map[string][]byte{}, map[string]string{}, map[string]string{}
	overlay(values, byFold, scopes, map[string][]byte{"Path": []byte("personal"), "KEEP": []byte("k")}, "personal")
	overlay(values, byFold, scopes, map[string][]byte{"PATH": []byte("device")}, "machine")
	var got []string
	for name, value := range values {
		got = append(got, name+"="+string(value))
	}
	sort.Strings(got)
	return got, scopes
}

// A device override named PATH replaces the personal Path on Windows (one variable, the override wins); on Linux and
// macOS the two are different variables and both are delivered.
func TestOverlayNamesDifferingOnlyByCaseAcrossLayers(t *testing.T) {
	got, scopes := overlaid(t, true)
	if want := []string{"KEEP=k", "PATH=device"}; !reflect.DeepEqual(got, want) || scopes["PATH"] != "machine" || scopes["Path"] != "" {
		t.Fatalf("case-insensitive: %v %v", got, scopes)
	}
	got, scopes = overlaid(t, false)
	if want := []string{"KEEP=k", "PATH=device", "Path=personal"}; !reflect.DeepEqual(got, want) || scopes["Path"] != "personal" {
		t.Fatalf("case-sensitive: %v %v", got, scopes)
	}
}

func TestMergeKeepsCaseSensitiveNamesApartOffWindows(t *testing.T) {
	prior := CaseInsensitiveNames
	t.Cleanup(func() { CaseInsensitiveNames = prior })
	CaseInsensitiveNames = false
	got, err := Merge([]string{"Foo=base"}, []string{"FOO=managed"})
	if err != nil || !reflect.DeepEqual(got, []string{"FOO=managed", "Foo=base"}) {
		t.Fatalf("%v %v", got, err)
	}
	CaseInsensitiveNames = true
	if got, err = Merge([]string{"Foo=base"}, []string{"FOO=managed"}); err != nil || !reflect.DeepEqual(got, []string{"Foo=managed"}) {
		t.Fatalf("%v %v", got, err)
	}
}
