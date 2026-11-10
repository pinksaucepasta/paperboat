package updated

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
)

// Release 2026.11.09.7 in a clean Windows Sandbox: the published one-liner installed, then pairing failed with
// "Windows installation failed while preparing updater state" (cause: "pb TUF target descriptor is invalid").
// The installation that awaits enrollment records no repository, and the empty one was sent to the TUF check.
func TestAnInstallationAwaitingEnrollmentIsVouchedForByTheBuildsOwnRepository(t *testing.T) {
	previous := buildinfo.DefaultReleaseURL
	t.Cleanup(func() { buildinfo.DefaultReleaseURL = previous })
	buildinfo.DefaultReleaseURL = "https://get.example.test"
	if got, err := recoveryRepository("", true); err != nil || got != "https://get.example.test/tuf" {
		t.Fatalf("awaiting enrollment: %q, %v", got, err)
	}
	if got, err := recoveryRepository("https://paired.example.test/tuf", false); err != nil || got != "https://paired.example.test/tuf" {
		t.Fatalf("paired: %q, %v", got, err)
	}
	// A paired installation that lost its repository is not silently pointed elsewhere.
	if got, err := recoveryRepository("", false); err != nil || got != "" {
		t.Fatalf("paired without a repository: %q, %v", got, err)
	}
	buildinfo.DefaultReleaseURL = ""
	if _, err := recoveryRepository("", true); err == nil {
		t.Fatal("a build without a release host vouched for an installation")
	}
}
