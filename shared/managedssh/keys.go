package managedssh

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"golang.org/x/crypto/ssh"
	"net/netip"
	"slices"
	"strings"
)

const MaxHostKeys = 8

// CATrustOptions are the options of a host's line trusting a user CA: certificates carrying principal (the machine's
// principal "pb:<machine>"), and only on the loopback connections the host's own bridge makes (BridgeSSH dials from
// LoopbackSource or ::1), so a certificate never opens a session that bypasses the stream (direct sshd over LAN or
// tailnet is refused at login). Callers check ValidCAPrincipal first.
func CATrustOptions(principal string) string {
	return `cert-authority,principals="` + principal + `",` + CATrustFrom
}

// CATrustFrom is the from= option of CATrustOptions; CertificateSourceAddress is the same range in every certificate.
const (
	CATrustFrom              = `from="127.0.0.0/8,::1"`
	CertificateSourceAddress = "127.0.0.0/8,::1/128"
)

// loopbackSourceSlots is how many source addresses the bridge spreads users over: macOS routes only aliased 127/8
// addresses, so its machine guard aliases each of LoopbackSources.
const loopbackSourceSlots = 64

// LoopbackSource is the IPv4 address the host's bridge connects to sshd from for one user (account ID). OpenSSH 9.8+
// PerSourcePenalties throttle a source address after failed logins; with one source per user, one user's failures
// never lock the others out of the machine's sshd. One /24 per slot keeps users apart even under
// PerSourceNetBlockSize 24. Users whose hashes collide share a slot (as all shared 127.0.0.1 before).
func LoopbackSource(accountID string) netip.Addr {
	sum := sha256.Sum256([]byte(accountID))
	return netip.AddrFrom4([4]byte{127, 0, sum[0] % loopbackSourceSlots, 200})
}

// LoopbackSources are every address LoopbackSource returns.
func LoopbackSources() []netip.Addr {
	out := make([]netip.Addr, loopbackSourceSlots)
	for slot := range out {
		out[slot] = netip.AddrFrom4([4]byte{127, 0, byte(slot), 200})
	}
	return out
}

// ValidCAPrincipal: a machine principal as the server makes it ("pb:<machine>"), safe inside principals="".
func ValidCAPrincipal(value string) bool {
	return strings.HasPrefix(value, "pb:") && len(value) <= 200 && !strings.ContainsAny(value, "\",\r\n\t\x00 *?!")
}

// MaxUserCAs is the most user CAs a host trusts: one pending, one active and one retiring during a rotation.
const MaxUserCAs = 3
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
