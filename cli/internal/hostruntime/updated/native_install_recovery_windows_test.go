//go:build windows

package updated

import (
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

func TestWindowsNativeInstallPinRequiresExactDeclaredBytesAndArguments(t *testing.T) {
	expected := windowsServiceTarget{Executable: `C:\Paperboat\versions\2026.10.08.32\pb.exe`, SHA256: "digest", Length: 123, Arguments: []string{"daemon", "__runtime-updated", "--instance", "owner"}}
	actual := expected
	identity := service.WindowsExecutableIdentity{Executable: expected.Executable, SHA256: expected.SHA256, Length: expected.Length}
	if !windowsNativeInstallPinMatches(expected, actual, identity) {
		t.Fatal("valid pin rejected")
	}
	wrong := identity
	wrong.SHA256 = "other"
	if windowsNativeInstallPinMatches(expected, actual, wrong) {
		t.Fatal("wrong bytes accepted")
	}
	actual.Arguments = append([]string(nil), expected.Arguments...)
	actual.Arguments[3] = "another-owner"
	if windowsNativeInstallPinMatches(expected, actual, identity) {
		t.Fatal("wrong owner accepted")
	}
}

func TestWindowsNativeInstallJournalBindsProtectedSourceBytesAndPlatform(t *testing.T) {
	journal := testWindowsFeatureJournal()
	config := WindowsConfig{Architecture: journal.Architecture, Source: installsource.Source{Version: journal.PreviousVersion, Platform: "windows", Architecture: journal.Architecture, SHA256: journal.PreviousBinary.SHA256, Length: journal.PreviousBinary.Length}}
	if err := validateWindowsNativeInstallJournalBinding(config, journal); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"version", "hash", "length", "architecture", "platform"} {
		bad := config
		switch kind {
		case "version":
			bad.Source.Version = "2026.10.08.99"
		case "hash":
			bad.Source.SHA256 = "wrong"
		case "length":
			bad.Source.Length++
		case "architecture":
			bad.Source.Architecture = "arm64"
		case "platform":
			bad.Source.Platform = "linux"
		}
		if err := validateWindowsNativeInstallJournalBinding(bad, journal); !errors.Is(err, errInvalidWindowsActivation) {
			t.Fatalf("%s accepted: %v", kind, err)
		}
	}
	config.Source.Version = journal.Version
	config.Source.SHA256 = journal.Runtime.SHA256
	config.Source.Length = journal.Runtime.Length
	if err := validateWindowsNativeInstallJournalBinding(config, journal); err != nil {
		t.Fatalf("Source-before-journal commit rejected: %v", err)
	}
}
