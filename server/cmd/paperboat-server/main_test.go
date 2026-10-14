package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunAdminRejectsUnknownOperation(t *testing.T) {
	err := runAdmin([]string{"usage-key", "revoke"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "usage-key provision") {
		t.Fatalf("error = %v", err)
	}
}

func TestServeRequiresWorkOSCredentials(t *testing.T) {
	t.Setenv("PAPERBOAT_WORKOS_CLIENT_ID", "")
	t.Setenv("PAPERBOAT_WORKOS_API_KEY", "")
	err := runServe(nil, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "PAPERBOAT_WORKOS_CLIENT_ID and PAPERBOAT_WORKOS_API_KEY") {
		t.Fatalf("serve without WorkOS credentials = %v", err)
	}
}

func TestRunAdminProvisionUsageKeyValidatesPublicKeyBeforeConfiguration(t *testing.T) {
	err := runAdmin([]string{
		"usage-key", "provision",
		"-public-key", "not-base64url!",
		"-not-before", "2026-07-22T00:00:00Z",
		"-expires-at", "2026-07-23T00:00:00Z",
	}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || err.Error() != "public-key must be unpadded base64url" {
		t.Fatalf("error = %v", err)
	}
}

func TestRunAdminProvisionUsageKeyValidatesTimestampsBeforeConfiguration(t *testing.T) {
	err := runAdmin([]string{
		"usage-key", "provision",
		"-public-key", strings.Repeat("A", 43),
		"-not-before", "yesterday",
		"-expires-at", "2026-07-23T00:00:00Z",
	}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || err.Error() != "not-before must be RFC3339" {
		t.Fatalf("error = %v", err)
	}
}
