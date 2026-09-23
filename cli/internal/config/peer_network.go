package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// PeerNetworkState is one atomic secure-store record. Keys are independent of
// Noise and QUIC identity. Pending custody survives an interrupted registration;
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
	if err != nil && !errors.Is(err, ErrSecretNotFound) {
		return ErrCredentialStoreUnavailable
	}
	state := PeerNetworkState{Version: 1}
	defer func() { clear(state.PrivateKey); clear(state.PendingKey) }()
	if err == nil {
		if len(previous) > 2048 || json.Unmarshal([]byte(previous), &state) != nil || !validPeerNetworkState(state) {
			return errors.New("peer network custody is invalid; recover endpoint identity")
		}
		canonical, err := json.Marshal(state)
		if err != nil || !bytes.Equal(canonical, []byte(previous)) {
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
		return ErrCredentialStoreUnavailable
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
