package service

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// What the renderers write is what DeclaredArguments reads back, including characters systemd and launchd treat
// specially.
func TestDeclaredArgumentsReadBackWhatTheRenderersWrote(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "pb")
	if err := os.WriteFile(executable, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	arguments := []string{"daemon", "--config", "/home/a b/50%/\"q\"/$HOME/c.json", "--server", "https://api.example.test"}
	for _, platform := range []string{"darwin", "linux"} {
		config := Config{Platform: platform, Kind: DaemonKind, ConfigRoot: t.TempDir(), Executable: executable, User: "u", Group: "g", Arguments: arguments, Environment: map[string]string{"HOME": "/home/u"}}
		body, err := (&Installer{config: config}).render()
		if err != nil {
			t.Fatal(platform, err)
		}
		path := filepath.Join(t.TempDir(), "definition")
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := DeclaredArguments(platform, path)
		if err != nil || !reflect.DeepEqual(got, arguments) {
			t.Fatalf("%s: got %q err=%v, want %q", platform, got, err, arguments)
		}
		if got, err := DeclaredArguments(platform, path+".absent"); got != nil || err != nil {
			t.Fatalf("%s: an absent definition read as %q %v", platform, got, err)
		}
	}
}
