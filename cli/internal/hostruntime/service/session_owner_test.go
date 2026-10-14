package service

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionOwnerIndependentDefinition(t *testing.T) {
	generation := strings.Repeat("a", 32)
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			cfg := Config{Platform: platform, Kind: SessionOwnerKind, Instance: "u501", SessionOwnerGeneration: generation, ConfigRoot: t.TempDir(), Executable: executable(t), User: "alice", Group: "staff", Arguments: []string{"daemon", "__runtime-session-owner", "--generation", generation}, Controller: &controller{}}
			owner, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			body, err := owner.render()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(filepath.Base(owner.DefinitionPath()), generation) || !strings.Contains(string(body), generation) {
				t.Fatal("owner generation omitted from native identity or command")
			}
			if platform == "linux" && (!strings.Contains(string(body), "RuntimeDirectoryMode=0700") || !strings.Contains(string(body), "KillMode=mixed")) {
				t.Fatal("private runtime or child containment lost")
			}
			other := cfg
			other.SessionOwnerGeneration = strings.Repeat("b", 32)
			other.Arguments = []string{"daemon", "__runtime-session-owner", "--generation", other.SessionOwnerGeneration}
			next, err := New(other)
			if err != nil {
				t.Fatal(err)
			}
			if owner.DefinitionPath() == next.DefinitionPath() {
				t.Fatal("generations share a native service")
			}
			for _, invalid := range []string{"", strings.Repeat("a", 31), strings.Repeat("a", 33), strings.Repeat("A", 32), "../../" + strings.Repeat("a", 26)} {
				bad := cfg
				bad.SessionOwnerGeneration = invalid
				if _, err := New(bad); !errors.Is(err, ErrInvalidDefinition) {
					t.Fatalf("generation %q accepted: %v", invalid, err)
				}
			}
			mismatch := cfg
			mismatch.Arguments = []string{"daemon", "__runtime-session-owner", "--generation", strings.Repeat("b", 32)}
			if _, err := New(mismatch); !errors.Is(err, ErrInvalidDefinition) {
				t.Fatal("mismatched generation command accepted")
			}
			foreign := cfg
			foreign.Kind = HostdKind
			if _, err := New(foreign); !errors.Is(err, ErrInvalidDefinition) {
				t.Fatal("generation accepted on another role")
			}
		})
	}
}
