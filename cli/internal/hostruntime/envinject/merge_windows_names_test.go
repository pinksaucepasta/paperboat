package envinject

import "testing"

// A Windows owner environment holds names like ProgramFiles(x86); the terminal launch merges managed ENV into it.
func TestMergeKeepsWindowsBaseNames(t *testing.T) {
	merged, err := Merge([]string{"ProgramFiles(x86)=C:\\Program Files (x86)", "Path=C:\\Windows"}, []string{"TOKEN=1"})
	if err != nil || len(merged) != 3 || merged[1] != "ProgramFiles(x86)=C:\\Program Files (x86)" {
		t.Fatalf("merged=%q err=%v", merged, err)
	}
	if _, err := Merge([]string{"A\nB=1"}, nil); err == nil {
		t.Fatal("accepted a malformed base name")
	}
	if _, err := Merge(nil, []string{"ProgramFiles(x86)=1"}); err == nil {
		t.Fatal("managed names must stay portable")
	}
}
