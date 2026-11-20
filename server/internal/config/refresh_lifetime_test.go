package config

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/peeridentity"
)

// A machine asleep for 31 days still holds a valid certificate; its CLI session must refresh, not sign in again.
func TestRefreshTokenOutlivesMachineCertificate(t *testing.T) {
	if got := Default().CLIAuth.RefreshTokenLifetime; got < peeridentity.MaxCertificateLifetime {
		t.Fatalf("refresh token lifetime %s is shorter than a machine certificate's %s", got, peeridentity.MaxCertificateLifetime)
	}
}
