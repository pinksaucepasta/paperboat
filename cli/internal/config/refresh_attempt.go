package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
)

// The original bearer remains in SecretStore until the successor is durably
// committed. Metadata contains only session identity and a secret reference.
type refreshAttempt struct {
	Version   int    `json:"version"`
	ID        string `json:"attempt_id"`
	SessionID string `json:"session_id"`
	SecretRef string `json:"secret_ref"`
	State     string `json:"state"`
}

func (s ProfileStore) refreshAttemptPath(issuer string) string {
	return s.profilePath(issuer) + ".refresh.json"
}

func (s ProfileStore) writeRefreshAttempt(issuer string, attempt refreshAttempt) error {
	body, err := json.Marshal(attempt)
	if err != nil {
		return err
	}
	return atomicWrite(s.refreshAttemptPath(issuer), body, 0600)
}

func (s ProfileStore) loadRefreshAttempt(issuer string) (refreshAttempt, error) {
	path := s.refreshAttemptPath(issuer)
	info, err := os.Lstat(path)
	if err != nil {
		return refreshAttempt{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return refreshAttempt{}, ErrCredentialStoreUnavailable
	}
	file, err := os.Open(path)
	if err != nil {
		return refreshAttempt{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return refreshAttempt{}, ErrCredentialStoreUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(body) > 4096 {
		return refreshAttempt{}, ErrCredentialStoreUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var attempt refreshAttempt
	var extra any
	if decoder.Decode(&attempt) != nil || decoder.Decode(&extra) != io.EOF {
		return refreshAttempt{}, ErrCredentialStoreUnavailable
	}
	decoded, err := hex.DecodeString(attempt.ID)
	if err != nil || len(decoded) != 32 || attempt.ID != strings.ToLower(attempt.ID) || attempt.Version != 1 || attempt.SessionID == "" || attempt.SecretRef != "refresh-attempt-"+profileKey(issuer)+"-"+attempt.ID || (attempt.State != "prepared" && attempt.State != "ready" && attempt.State != "committed") {
		return refreshAttempt{}, ErrCredentialStoreUnavailable
	}
	return attempt, nil
}

func (s ProfileStore) clearRefreshAttempt(issuer string, attempt refreshAttempt) error {
	// Persist cleanup ownership before deleting the original bearer. A crash or
	// failed removal then retries cleanup without reusing a consumed credential.
	if attempt.State != "committed" {
		attempt.State = "committed"
		if err := s.writeRefreshAttempt(issuer, attempt); err != nil {
			return err
		}
	}
	if err := s.Secrets.Delete(attempt.SecretRef); err != nil && !credentialAbsenceOnly(err) {
		return err
	}
	if err := os.Remove(s.refreshAttemptPath(issuer)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s ProfileStore) prepareRefreshAttempt(p Profile, credential Credential, attempt refreshAttempt) (refreshAttempt, Credential, error) {
	if attempt.ID == "" {
		var nonce [32]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return refreshAttempt{}, Credential{}, err
		}
		attempt = refreshAttempt{Version: 1, ID: hex.EncodeToString(nonce[:]), SessionID: p.CLIClientSessionID, State: "prepared"}
		attempt.SecretRef = "refresh-attempt-" + profileKey(p.Issuer) + "-" + attempt.ID
		if err := s.writeRefreshAttempt(p.Issuer, attempt); err != nil {
			return refreshAttempt{}, Credential{}, err
		}
	}
	if attempt.State == "prepared" {
		if err := s.Secrets.Set(attempt.SecretRef, credential.RefreshToken); err != nil {
			return refreshAttempt{}, Credential{}, err
		}
		attempt.State = "ready"
		if err := s.writeRefreshAttempt(p.Issuer, attempt); err != nil {
			return refreshAttempt{}, Credential{}, err
		}
	}
	original, err := s.Secrets.Get(attempt.SecretRef)
	if err != nil {
		return refreshAttempt{}, Credential{}, err
	}
	if original == "" {
		return refreshAttempt{}, Credential{}, ErrCredentialStoreUnavailable
	}
	credential.RefreshToken = original
	return attempt, credential, nil
}
