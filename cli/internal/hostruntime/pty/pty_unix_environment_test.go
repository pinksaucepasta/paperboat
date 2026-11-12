//go:build darwin || linux

package pty

import "testing"

// Linux and macOS keep environment names case-sensitive: Path and PATH are two variables (a personal value and a
// device override may differ only by case); an exact repeat is still refused.
func TestValidEnvironmentKeepsNamesDifferingOnlyByCaseApartOnUnix(t *testing.T) {
	if !validEnvironment([]string{"XPC_Case=personal", "XPC_CASE=override"}) {
		t.Fatal("names differing only by case were refused on a case-sensitive OS")
	}
	if validEnvironment([]string{"XPC_CASE=a", "XPC_CASE=b"}) {
		t.Fatal("an exactly repeated name was accepted")
	}
}
