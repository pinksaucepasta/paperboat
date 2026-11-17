// Package hoststate owns the durable local cache used by the Paperboat host
// runtime to resume tunnel reconciliation. The server remains authoritative.
package hoststate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	sharedprotocol "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"regexp"
)

const (
	Schema        = "paperboat.host-state"
	SchemaVersion = 1
	MaxStateBytes = 8 << 20

	maxTunnels        = 1_000
	maxRoutes         = 20_000
	maxConnectors     = 10_000
	maxJournalEntries = 2_000
	maxSnapshotBytes  = 4 << 20
)

var (
	ErrInvalidState         = errors.New("invalid host state")
	ErrCredentialMaterial   = errors.New("reusable credential material is forbidden in host state")
	idPattern               = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	stableEndpointIDPattern = regexp.MustCompile(`^endpoint_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

type ConfigSnapshot struct {
	TunnelID    string          `json:"tunnel_id"`
	Generation  uint64          `json:"generation"`
	ContentHash string          `json:"content_hash"`
	Payload     json.RawMessage `json:"payload"`
}

// TunnelConfigSnapshot is the only configuration payload accepted by the
// durable host cache. It mirrors the connector-v1 wire vocabulary emitted by
// paperboat-server. References may identify a platform credential, but never
// contain reusable credential material.
type TunnelConfigSnapshot = sharedprotocol.ConfigSnapshot

type TunnelConfigRoute = sharedprotocol.ConfigRoute

// NewConfigSnapshot canonicalizes the JSON before hashing it. It rejects
// preview leases and secret-bearing fields so durable host state cannot become
// an alternate plaintext credential store.
func NewConfigSnapshot(tunnelID string, generation uint64, payload []byte) (ConfigSnapshot, error) {
	if !validID(tunnelID) || generation == 0 || len(payload) == 0 || len(payload) > maxSnapshotBytes {
		return ConfigSnapshot{}, ErrInvalidState
	}
	canonical, err := canonicalSnapshot(payload)
	if err != nil {
		return ConfigSnapshot{}, err
	}
	if _, err := parseTunnelConfigSnapshot(canonical, tunnelID, generation); err != nil {
		return ConfigSnapshot{}, err
	}
	digest := sha256.Sum256(canonical)
	return ConfigSnapshot{
		TunnelID: tunnelID, Generation: generation,
		ContentHash: "sha256:" + hex.EncodeToString(digest[:]),
		Payload:     append(json.RawMessage(nil), canonical...),
	}, nil
}

// ParseTunnelConfigSnapshot strictly decodes the canonical server snapshot
// and verifies the expected tunnel and generation. Callers should hash the
// returned canonical bytes through NewConfigSnapshot rather than re-encoding
// the decoded struct.
func ParseTunnelConfigSnapshot(payload []byte, tunnelID string, generation uint64) (TunnelConfigSnapshot, error) {
	if !validID(tunnelID) || generation == 0 || len(payload) == 0 || len(payload) > maxSnapshotBytes {
		return TunnelConfigSnapshot{}, ErrInvalidState
	}
	canonical, err := canonicalSnapshot(payload)
	if err != nil {
		return TunnelConfigSnapshot{}, err
	}
	return parseTunnelConfigSnapshot(canonical, tunnelID, generation)
}

// parseTunnelConfigSnapshot is the shared validator (connectorprotocol.ParseConfigSnapshot) with this package's errors.
func parseTunnelConfigSnapshot(canonical []byte, tunnelID string, generation uint64) (TunnelConfigSnapshot, error) {
	if len(canonical) == 0 {
		return TunnelConfigSnapshot{}, ErrInvalidState
	}
	snapshot, err := sharedprotocol.ParseConfigSnapshot(canonical, tunnelID, generation)
	if err != nil {
		return TunnelConfigSnapshot{}, snapshotError(err)
	}
	return snapshot, nil
}

// canonicalSnapshot is the shared canonical form (connectorprotocol.CanonicalConfigSnapshot) with this package's errors.
func canonicalSnapshot(payload []byte) ([]byte, error) {
	canonical, err := sharedprotocol.CanonicalConfigSnapshot(payload)
	if err != nil {
		return nil, snapshotError(err)
	}
	return canonical, nil
}

func snapshotError(err error) error {
	if errors.Is(err, sharedprotocol.ErrSnapshotCredentialMaterial) {
		return safeStoreFailure("tunnel configuration contains an unsafe credential reference", ErrCredentialMaterial, err)
	}
	return safeStoreFailure("tunnel configuration snapshot is invalid", ErrInvalidState, err)
}

// ValidateStableEndpointID accepts only the canonical endpoint_UUIDv4 used as
// the immutable managed tunnel endpoint identity. Host runtime state must not
// invent this identity from a display name, host name, or endpoint hash.
func ValidateStableEndpointID(value string) error {
	if !stableEndpointIDPattern.MatchString(value) {
		return ErrInvalidState
	}
	return nil
}

func validID(value string) bool { return idPattern.MatchString(value) }
