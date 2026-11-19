package managedssh

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"net"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

var ErrKnownHosts = errors.New("managed SSH known-host authority is invalid")

// FormatKnownHosts names the host as ssh looks it up without a HostKeyAlias: bare for port 22, [host]:port otherwise.
func FormatKnownHosts(host string, port uint16, keys []HostPublicKey) ([]byte, error) {
	return formatKnownHosts(host, port, false, keys)
}

// FormatKnownHostsForAlias names the host as ssh looks it up under a HostKeyAlias: the alias alone, whatever the port
// (the Windows loopback connection of a machine whose sshd is not on port 22, victus 2026-11-19).
func FormatKnownHostsForAlias(host string, port uint16, keys []HostPublicKey) ([]byte, error) {
	return formatKnownHosts(host, port, true, keys)
}

func formatKnownHosts(host string, port uint16, alias bool, keys []HostPublicKey) ([]byte, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if !validSSHHostName(host) || port == 0 || len(keys) == 0 || len(keys) > 16 {
		return nil, ErrKnownHosts
	}
	target := knownhosts.Normalize(net.JoinHostPort(host, strconv.Itoa(int(port))))
	if alias {
		target = host
	}
	var result bytes.Buffer
	seen := make(map[[32]byte]bool, len(keys))
	for _, key := range keys {
		public, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(key.PublicKey))
		fingerprint := sha256.Sum256(nil)
		if err == nil {
			fingerprint = sha256.Sum256(public.Marshal())
		}
		if err != nil || !validHostKeyAlgorithm(public.Type()) || key.Algorithm != public.Type() || fingerprint != key.Fingerprint || seen[fingerprint] || len(options) != 0 || len(strings.TrimSpace(string(rest))) != 0 || strings.ContainsAny(comment, "\r\n\x00") {
			return nil, ErrKnownHosts
		}
		seen[fingerprint] = true
		result.WriteString(target)
		result.WriteByte(' ')
		result.Write(bytes.TrimSpace(ssh.MarshalAuthorizedKey(public)))
		result.WriteByte('\n')
	}
	return result.Bytes(), nil
}

func validSSHHostName(value string) bool {
	if value == "" || len(value) > 253 || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") || net.ParseIP(value) != nil || strings.ContainsAny(value, "\r\n\x00[]: ") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if character != '-' && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
				return false
			}
		}
	}
	return true
}
