package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildValidatesAndGeneratesCanonicalMatrix(t *testing.T) {
	manifestPath, sourcePath := writeFixture(t, `{
  "schema_version": 1,
  "shards": [{"name":"core","platform":"linux","timeout_seconds":60,"tests":["TestOne","TestTwo"]}]
}`)
	data, err := build(manifestPath, sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	want := `"run": "^(TestOne|TestTwo)$"`
	if !strings.Contains(string(data), want) || data[len(data)-1] != '\n' {
		t.Fatalf("generated matrix is not canonical: %s", data)
	}
}

func TestBuildRejectsInvalidAssignments(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     string
	}{
		{
			name: "duplicate",
			manifest: `{"schema_version":1,"shards":[
{"name":"a","platform":"linux","timeout_seconds":60,"tests":["TestOne"]},
{"name":"b","platform":"linux","timeout_seconds":60,"tests":["TestOne","TestTwo"]}]}`,
			want: "assigned to both",
		},
		{
			name:     "missing",
			manifest: `{"schema_version":1,"shards":[{"name":"a","platform":"linux","timeout_seconds":60,"tests":["TestOne"]}]}`,
			want:     "tests missing from bounded shards: TestTwo",
		},
		{
			name:     "platform",
			manifest: `{"schema_version":1,"shards":[{"name":"a","platform":"windows","timeout_seconds":60,"tests":["TestOne","TestTwo"]}]}`,
			want:     "unsupported platform",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifestPath, sourcePath := writeFixture(t, test.manifest)
			_, err := build(manifestPath, sourcePath)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestBuildRejectsDuplicateAndTrailingManifestValues(t *testing.T) {
	valid := `{"schema_version":1,"shards":[{"name":"core","platform":"linux","timeout_seconds":60,"tests":["TestOne","TestTwo"]}]}`
	for name, manifest := range map[string]string{
		"duplicate field": strings.Replace(valid, `"schema_version":1`, `"schema_version":2,"schema_version":1`, 1),
		"trailing value":  valid + ` {"schema_version":1,"shards":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			manifestPath, sourcePath := writeFixture(t, manifest)
			if _, err := build(manifestPath, sourcePath); err == nil {
				t.Fatal("ambiguous manifest was accepted")
			}
		})
	}
}

func writeFixture(t *testing.T, manifest string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "scenarios.json")
	sourcePath := filepath.Join(dir, "topology_test.go")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("package topology\nfunc TestOne() {}\nfunc TestTwo() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return manifestPath, sourcePath
}
