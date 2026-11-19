package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// executableSHA256 is the SHA-256 of the running pb executable, computed once at daemon start and reported with the
// version so the server can check the build against the hash of a signed release target. A read failure reports
// nothing: the server then counts the build as non-official.
func executableSHA256() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	return fileSHA256(path)
}

func fileSHA256(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return ""
	}
	return hex.EncodeToString(hash.Sum(nil))
}
