package localdaemon

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// Each local daemon generation runs in its own user unit, named by the first 32 hex of its
// executable's SHA-256, the same name the root install gives carrier and hostd generations
// (hostinstall.sourceGeneration). An upgrade starts the new unit and sends the old one SIGQUIT,
// so the service manager never kills a daemon that still carries forwarded connections.

// DaemonGeneration names the generation of a daemon executable.
func DaemonGeneration(executable string) (string, error) {
	file, err := os.Open(executable)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil))[:32], nil
}
