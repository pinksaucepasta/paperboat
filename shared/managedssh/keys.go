package managedssh

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"golang.org/x/crypto/ssh"
	"slices"
	"strings"
)

const MaxHostKeys = 8
const MaxHostKeyBytes = 8192

var ErrInvalid = errors.New("managed SSH request is invalid")

type PublicKey struct {
	Fingerprint [32]byte
	Algorithm   string
	PublicKey   string
}

func ParsePublicKey(raw string, client bool) (PublicKey, error) {
	if len(raw) < 32 || len(raw) > MaxHostKeyBytes || strings.ContainsAny(raw, "\r\x00") {
		return PublicKey{}, ErrInvalid
	}
	public, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(raw))
	if err != nil || len(options) != 0 || len(strings.TrimSpace(string(rest))) != 0 || strings.ContainsAny(comment, "\r\n\x00") {
		return PublicKey{}, ErrInvalid
	}
	algorithm := public.Type()
	if client && algorithm != ssh.KeyAlgoED25519 || !client && !validHostAlgorithm(algorithm) {
		return PublicKey{}, ErrInvalid
	}
	canonical := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public)))
	return PublicKey{Fingerprint: sha256.Sum256(public.Marshal()), Algorithm: algorithm, PublicKey: canonical}, nil
}

func fingerprintSet(keys []PublicKey) [32]byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte("paperboat-ssh-host-key-set-v1"))
	for _, key := range keys {
		var length [2]byte
		binary.BigEndian.PutUint16(length[:], uint16(len(key.PublicKey)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(key.PublicKey))
	}
	var result [32]byte
	copy(result[:], hash.Sum(nil))
	return result
}

func validHostAlgorithm(value string) bool {
	switch value {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521, ssh.KeyAlgoRSA:
		return true
	default:
		return false
	}
}

// HostKeyInventory is a canonical, duplicate-free host-key set.
type HostKeyInventory struct {
	Keys        []PublicKey
	Fingerprint [32]byte
}

func ParseHostPublicKeys(values []string) (HostKeyInventory, error) {
	if len(values) == 0 || len(values) > MaxHostKeys {
		return HostKeyInventory{}, ErrInvalid
	}
	keys := make([]PublicKey, 0, len(values))
	seen := make(map[[32]byte]bool, len(values))
	for _, value := range values {
		key, err := ParsePublicKey(value, false)
		if err != nil || seen[key.Fingerprint] {
			return HostKeyInventory{}, ErrInvalid
		}
		seen[key.Fingerprint] = true
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b PublicKey) int { return strings.Compare(string(a.Fingerprint[:]), string(b.Fingerprint[:])) })
	return HostKeyInventory{Keys: keys, Fingerprint: fingerprintSet(keys)}, nil
}
