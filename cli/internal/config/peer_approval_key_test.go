package config

import (
	"bytes"
	"errors"
	"maps"
	"path/filepath"
	"testing"
)

func TestPeerApprovalSigningKeyPreservesCustody(t *testing.T) {
	const issuer, account, endpoint = "https://api.example.test", "account_1", "cli_1"
	for _, name := range []string{"absent", "account signer", "fresh signer", "fresh preferred", "malformed fresh", "incomplete fresh", "different session", "different issuer"} {
		t.Run(name, func(t *testing.T) {
			secrets := &peerTestSecretStore{values: map[string]string{}}
			store := ProfileStore{Path: filepath.Join(t.TempDir(), "profile"), Secrets: secrets}
			var expected []byte
			if name == "account signer" || name == "fresh preferred" || name == "malformed fresh" || name == "incomplete fresh" {
				keys, err := store.PeerIdentityKeys(issuer, account, endpoint)
				if err != nil {
					t.Fatal(err)
				}
				expected = append([]byte(nil), keys.RootPrivate...)
				clearPeerIdentity(&keys)
			}
			if name != "absent" && name != "account signer" {
				keys, err := store.FreshPeerIdentityKeys(issuer, account, endpoint)
				if err != nil {
					t.Fatal(err)
				}
				clear(expected)
				expected = append([]byte(nil), keys.RootPrivate...)
				clearPeerIdentity(&keys)
			}
			defer clear(expected)
			if name == "malformed fresh" {
				secrets.values[peerIdentitySecretRef(issuer, endpoint, "endpoint-signing")] = "invalid"
			}
			if name == "incomplete fresh" {
				delete(secrets.values, peerIdentitySecretRef(issuer, endpoint, "endpoint-quic"))
			}
			before := maps.Clone(secrets.values)
			writes := secrets.setCount
			requestedIssuer, requestedEndpoint := issuer, endpoint
			if name == "different session" {
				requestedEndpoint = "cli_other"
			}
			if name == "different issuer" {
				requestedIssuer = "https://other.example.test"
			}
			key, err := store.PeerApprovalSigningKey(requestedIssuer, account, requestedEndpoint)
			defer clear(key)
			switch name {
			case "absent", "different session", "different issuer":
				if !errors.Is(err, ErrSecretNotFound) || key != nil {
					t.Fatal("missing signer was not rejected")
				}
			case "malformed fresh", "incomplete fresh":
				if err == nil || key != nil {
					t.Fatal("invalid session identity fell back to account signer")
				}
			default:
				if err != nil || !bytes.Equal(key, expected) {
					t.Fatalf("existing signer unavailable: %v", err)
				}
			}
			if !maps.Equal(before, secrets.values) || secrets.setCount != writes {
				t.Fatal("read-only signer lookup changed key custody")
			}
		})
	}
}
