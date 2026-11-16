package managedssh

import (
	"encoding/binary"
	"slices"
	"strings"
)

// MaxRevokedKeyIDs bounds a machine's revoked list: certificates live 12 h and one CLI session holds one key ID per
// machine, so this is how many sessions may lose access to one machine within a certificate's lifetime.
const MaxRevokedKeyIDs = 4096

// KRL is an OpenSSH key revocation list (PROTOCOL.krl, read by sshd's RevokedKeys and `ssh-keygen -Q`) revoking the
// certificates with the given key IDs signed by any of the given CAs. Key IDs, not serials: one CLI session's key ID
// for a machine is stable ("paperboat:<user>:<session>:<machine>"), so revoking a session revokes every certificate it
// was issued for that machine, like a Teleport lock on a session (~/ref/teleport api/types/lock.go). version must
// grow with every change. With no key IDs the list is valid and revokes nothing.
func KRL(caKeys [][]byte, keyIDs []string, version, generated uint64) []byte {
	ids := slices.Clone(keyIDs)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	out := []byte("SSHKRL\n\x00")
	out = binary.BigEndian.AppendUint32(out, 1) // format version
	out = binary.BigEndian.AppendUint64(out, version)
	out = binary.BigEndian.AppendUint64(out, generated)
	out = binary.BigEndian.AppendUint64(out, 0) // flags
	out = appendSSHString(out, nil)             // reserved
	out = appendSSHString(out, []byte("Paperboat managed SSH revocations"))
	if len(ids) == 0 {
		return out
	}
	var keyIDSection []byte
	for _, id := range ids {
		keyIDSection = appendSSHString(keyIDSection, []byte(id))
	}
	for _, ca := range caKeys {
		var certificates []byte
		certificates = appendSSHString(certificates, ca)
		certificates = appendSSHString(certificates, nil) // reserved
		certificates = append(certificates, 0x23)         // KRL_SECTION_CERT_KEY_ID
		certificates = appendSSHString(certificates, keyIDSection)
		out = append(out, 1) // KRL_SECTION_CERTIFICATES
		out = appendSSHString(out, certificates)
	}
	return out
}

// ValidRevokedKeyID is a key ID the server issues: "paperboat:" and printable, no line breaks.
func ValidRevokedKeyID(value string) bool {
	return strings.HasPrefix(value, "paperboat:") && len(value) <= 512 && !strings.ContainsAny(value, "\r\n\x00")
}

func appendSSHString(out, value []byte) []byte {
	out = binary.BigEndian.AppendUint32(out, uint32(len(value)))
	return append(out, value...)
}
