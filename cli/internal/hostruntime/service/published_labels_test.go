package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// publishedAs writes text as the declaration an earlier build published for config and returns this build's installer for it.
func publishedAs(t *testing.T, config Config, text string) *Installer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "declaration")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Installer{config: config, definitionPath: path}
}

func rendered(t *testing.T, config Config) string {
	t.Helper()
	body, err := (&Installer{config: config}).render()
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// A label change must not break the published-definition check of the next build (252b2ada1 changed the Windows labels; every
// install over a build that published the old ones failed "invalid service definition", Windows test machine 2026-11-19).
func TestLinuxPublishedUnitIgnoresItsDescription(t *testing.T) {
	config := Config{Platform: "linux", Kind: WorkerKind, User: "paperboat", Group: "paperboat", Executable: "/usr/local/libexec/paperboat/pb"}
	current := rendered(t, config)
	relabelled := strings.Replace(current, "Description=Paperboat host runtime", "Description=An older label", 1)
	if relabelled == current {
		t.Fatal("the fixture did not change the description")
	}
	if err := publishedAs(t, config, relabelled).VerifyPublishedDefinition(); err != nil {
		t.Fatalf("a unit that differs only in its description was refused: %v", err)
	}
	other := strings.Replace(current, `ExecStart="/usr/local/libexec/paperboat/pb"`, `ExecStart="/tmp/other"`, 1)
	if err := publishedAs(t, config, other).VerifyPublishedDefinition(); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("a unit for another executable was accepted: %v", err)
	}
}

// A launchd plist has no label meant for people: it is compared as written, so what it runs is checked and nothing is relaxed.
func TestMacOSPublishedPlistIsComparedAsWritten(t *testing.T) {
	config := Config{Platform: "darwin", Kind: DaemonKind, ConfigRoot: "/Users/t", Executable: "/Users/t/.local/bin/pb", User: "t", Group: "staff", Arguments: []string{"daemon"}}
	current := rendered(t, config)
	if err := publishedAs(t, config, current).VerifyPublishedDefinition(); err != nil {
		t.Fatalf("the plist this build renders was refused: %v", err)
	}
	other := strings.Replace(current, "<string>daemon</string>", "<string>other</string>", 1)
	if err := publishedAs(t, config, other).VerifyPublishedDefinition(); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("a plist for other arguments was accepted: %v", err)
	}
}

func TestPublishedDefinitionIgnoresItsDisplayLabels(t *testing.T) {
	old := []byte(`{"schema":"paperboat.windows-service/v1","name":"PaperboatCarrier-u1-g1","display_name":"PaperboatCarrier-u1-g1","description":"old","executable":"C:\\pb.exe","arguments":["__carrier"]}`)
	current := []byte(`{"schema":"paperboat.windows-service/v1","name":"PaperboatCarrier-u1-g1","display_name":"Paperboat (carrier)","description":"new","executable":"C:\\pb.exe","arguments":["__carrier"]}`)
	if !sameDefinition(old, current) {
		t.Fatal("a declaration that differs only in its display labels was refused")
	}
	other := []byte(`{"schema":"paperboat.windows-service/v1","name":"PaperboatCarrier-u1-g1","display_name":"Paperboat (carrier)","description":"new","executable":"C:\\other.exe","arguments":["__carrier"]}`)
	if sameDefinition(old, other) {
		t.Fatal("a declaration for another executable was accepted")
	}
}
