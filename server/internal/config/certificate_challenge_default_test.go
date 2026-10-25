package config

import "testing"

// A certificates configuration with no challenge (written before the switch, or a fixture) means dns-01.
func TestCertificateChallengeDefaultsToDNS01(t *testing.T) {
	if got := (Certificates{}).ChallengeType(); got != CertificateChallengeDNS01 {
		t.Fatalf("empty challenge = %q, want %q", got, CertificateChallengeDNS01)
	}
	if got := (Certificates{Challenge: CertificateChallengeHTTP01}).ChallengeType(); got != CertificateChallengeHTTP01 {
		t.Fatalf("http-01 = %q", got)
	}
}
