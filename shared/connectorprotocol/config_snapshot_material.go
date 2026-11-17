package connectorprotocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
)

// ErrSnapshotCredentialMaterial marks a snapshot that carries reusable credential material (it wraps
// ErrSnapshotRejected): durable machine state and the control plane's store must never become a credential store.
var ErrSnapshotCredentialMaterial = errors.New("tunnel configuration snapshot contains credential material")

// CanonicalConfigSnapshot is the compact sorted-key JSON the snapshot's content hash covers: duplicate keys, deep
// nesting, trailing data, preview leases and credential material are refused before any hashing.
func CanonicalConfigSnapshot(payload []byte) ([]byte, error) {
	if err := rejectSnapshotCredentialMaterial(payload); err != nil {
		return nil, err
	}
	return canonicalJSON(payload, MaxSnapshotBytes)
}

// rejectSnapshotCredentialMaterial refuses a preview lease and any member that names reusable credential material
// (a credential reference must be a safe paperboat:// handle).
func rejectSnapshotCredentialMaterial(payload []byte) error {
	if len(payload) == 0 || len(payload) > MaxSnapshotBytes {
		return ErrSnapshotRejected
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	value, err := decodeValue(decoder, 0)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSnapshotRejected, err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrSnapshotRejected
	}
	if err := rejectDuplicateKeys(payload); err != nil {
		return fmt.Errorf("%w: %w", ErrSnapshotRejected, err)
	}
	if object, ok := value.(map[string]any); !ok || len(object) == 0 {
		return ErrSnapshotRejected
	}
	return visitSnapshotCredentialMaterial(value)
}

func visitSnapshotCredentialMaterial(value any) error {
	switch typed := value.(type) {
	case []any:
		for _, child := range typed {
			if err := visitSnapshotCredentialMaterial(child); err != nil {
				return err
			}
		}
	case map[string]any:
		for key, child := range typed {
			normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
			if normalized == "kind" && child == "preview_lease" {
				return fmt.Errorf("%w: preview leases are not durable", ErrSnapshotRejected)
			}
			if snapshotSecretReferenceKey(normalized) {
				if child == nil {
					continue
				}
				if reference, ok := child.(string); !ok || !safeSnapshotCredentialReference(reference) {
					return fmt.Errorf("%w: %w", ErrSnapshotRejected, ErrSnapshotCredentialMaterial)
				}
			}
			if snapshotForbiddenCredentialKey(normalized) {
				return fmt.Errorf("%w: %w", ErrSnapshotRejected, ErrSnapshotCredentialMaterial)
			}
			if err := visitSnapshotCredentialMaterial(child); err != nil {
				return err
			}
		}
	}
	return nil
}

// safeSnapshotCredentialReference is the one shape of a credential reference in a snapshot: a clean paperboat handle.
func safeSnapshotCredentialReference(reference string) bool {
	if len(reference) == 0 || len(reference) > MaxCredentialRef || strings.TrimSpace(reference) != reference {
		return false
	}
	parsed, err := url.Parse(reference)
	if err != nil || parsed.User != nil || parsed.Opaque != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Host != "paperboat" || parsed.Path == "" || parsed.Path == "/" || path.Clean(parsed.Path) != parsed.Path {
		return false
	}
	switch parsed.Scheme {
	case "keychain", "credential-manager", "secret-service", "protected-file", "tpm":
		return true
	}
	return false
}

func snapshotSecretReferenceKey(key string) bool {
	if strings.Contains(key, "credential_reference") {
		return true
	}
	base, found := strings.CutSuffix(key, "_reference")
	return found && snapshotForbiddenCredentialKey(base)
}

func snapshotForbiddenCredentialKey(key string) bool {
	if strings.HasSuffix(key, "_reference") {
		return false
	}
	switch key {
	case "authorization", "proxy_authorization", "token", "access_token", "refresh_token", "bearer_token", "api_key", "password", "secret", "credential", "credential_secret", "private_key", "client_secret":
		return true
	}
	return strings.HasSuffix(key, "_token") || strings.HasSuffix(key, "_password") || strings.HasSuffix(key, "_secret") || strings.HasSuffix(key, "_private_key")
}

// validSnapshotMatchHostname is the one shape of a route's match hostname or wildcard suffix.
func validSnapshotMatchHostname(value string) bool {
	return len(value) <= 253 && strings.TrimSpace(value) == value && value == strings.ToLower(value) && !strings.HasSuffix(value, ".") && !strings.Contains(value, "*") && wireHostnamePattern(value)
}
