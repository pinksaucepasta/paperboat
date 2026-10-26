package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// PeerNetworkState is one atomic secure-store record. Keys are independent of
// QUIC identity. Pending custody survives an interrupted registration;
// the signed configuration decides when it becomes the active key.
type PeerNetworkState struct {
	Version        int    `json:"version"`
	PrivateKey     []byte `json:"private_key,omitempty"`
	PendingKey     []byte `json:"pending_key,omitempty"`
	KeyGeneration  uint64 `json:"key_generation"`
	Generation     uint64 `json:"generation"`
	ConfigHash     string `json:"config_hash"`
	VirtualAddress string `json:"virtual_address"`
}

func (s PeerNetworkState) String() string { return "[peer network custody]" }

// UpdatePeerNetworkState serializes custody and configuration high-water updates
// across processes. A failed callback writes nothing. A failed durable write must
// prevent the caller from installing the corresponding network configuration.
func (s ProfileStore) UpdatePeerNetworkState(issuer, accountID, endpointID string, update func(*PeerNetworkState) error) (resultErr error) {
	if s.Path == "" || s.Secrets == nil || update == nil || !validCredentialID(accountID) || !validCredentialID(endpointID) {
		return ErrCredentialStoreUnavailable
	}
	issuer, err := NormalizeIssuer(issuer)
	if err != nil {
		return err
	}
	lock := newSharedLock(s.profilePath(issuer) + ".peer-network.lock")
	if err := lock.Lock(); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Unlock()) }()
	digest := sha256.Sum256([]byte(issuer + "\x00" + accountID + "\x00" + endpointID))
	ref := "peer-network-v1-" + hex.EncodeToString(digest[:])
	previous, err := s.Secrets.Get(ref)
	if err != nil && !credentialAbsenceOnly(err) {
		return credentialStoreFailure("peer network custody could not be loaded", err)
	}
	state := PeerNetworkState{Version: 1}
	defer func() { clear(state.PrivateKey); clear(state.PendingKey) }()
	if err == nil {
		if len(previous) > 2048 {
			return errors.New("peer network custody is invalid; recover endpoint identity")
		}
		if err := json.Unmarshal([]byte(previous), &state); err != nil {
			return safeConfigCause("peer network custody is invalid; recover endpoint identity", err)
		}
		if !validPeerNetworkState(state) {
			return errors.New("peer network custody is invalid; recover endpoint identity")
		}
		canonical, err := json.Marshal(state)
		if err != nil {
			return safeConfigCause("peer network custody could not be validated", err)
		}
		if !bytes.Equal(canonical, []byte(previous)) {
			return errors.New("peer network custody is not canonical")
		}
	}
	oldGeneration, oldHash := state.Generation, state.ConfigHash
	if err := update(&state); err != nil {
		return err
	}
	if !validPeerNetworkState(state) || state.Generation < oldGeneration || state.Generation == oldGeneration && state.ConfigHash != oldHash {
		return errors.New("peer network custody update would roll back authority")
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	defer clear(encoded)
	if string(encoded) == previous {
		return nil
	}
	if err := s.Secrets.Set(ref, string(encoded)); err != nil {
		return credentialStoreFailure("peer network custody could not be stored", err)
	}
	return nil
}

func validPeerNetworkState(s PeerNetworkState) bool {
	if s.Version != 1 || len(s.PrivateKey) != 0 && len(s.PrivateKey) != 32 || len(s.PendingKey) != 0 && len(s.PendingKey) != 32 {
		return false
	}
	if (s.KeyGeneration == 0) != (len(s.PrivateKey) == 0) {
		return false
	}
	if s.Generation == 0 {
		return s.ConfigHash == ""
	}
	hash, err := hex.DecodeString(s.ConfigHash)
	return err == nil && len(hash) == 32 && hex.EncodeToString(hash) == s.ConfigHash && len(s.PrivateKey) == 32
}

// The last verified signed peer network configuration is kept in its own owner-only file beside the profile,
// so a daemon restarted while Paperboat is unreachable keeps its peers (F21-01). Integrity comes from custody:
// the reader installs it only if its body hash equals PeerNetworkState.ConfigHash.
func (s ProfileStore) peerNetworkConfigurationPath(issuer, accountID, endpointID string) (string, error) {
	if s.Path == "" || !validCredentialID(accountID) || !validCredentialID(endpointID) {
		return "", errors.New("peer network configuration path is invalid")
	}
	digest := sha256.Sum256([]byte(issuer + "\x00" + accountID + "\x00" + endpointID))
	return filepath.Join(s.Path, "peer-network-config-v1-"+hex.EncodeToString(digest[:])+".jwt"), nil
}

// SavePeerNetworkConfiguration replaces the persisted configuration with token, atomically, mode 0600.
func (s ProfileStore) SavePeerNetworkConfiguration(issuer, accountID, endpointID, token string) error {
	path, err := s.peerNetworkConfigurationPath(issuer, accountID, endpointID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Path, 0o700); err != nil {
		return err
	}
	return writeCredentialFile(path, []byte(token))
}

// LoadPeerNetworkConfiguration reads the persisted configuration of at most limit bytes; "" when none.
func (s ProfileStore) LoadPeerNetworkConfiguration(issuer, accountID, endpointID string, limit int) (string, error) {
	path, err := s.peerNetworkConfigurationPath(issuer, accountID, endpointID)
	if err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return "", err
	}
	if len(data) > limit {
		return "", errors.New("persisted peer network configuration is too large")
	}
	return string(data), nil
}
