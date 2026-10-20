package inbox

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxDeliveredNames bounds the " (n)" suffixes tried when a name is taken.
const maxDeliveredNames = 10000

// ErrDeliveredDigest means the copied bytes are not the bytes the caller measured.
var ErrDeliveredDigest = errors.New("delivered file does not match its digest")

// Deliver copies one file into the Inbox folder the way a received file arrives: it appears
// complete or not at all (written under a hidden name, checked against its digest, then linked in),
// is readable only by the owner, and never replaces a file already there (a taken name gets " (2)",
// " (3)" ... before the extension). It returns the path the file was given.
func Deliver(root, name string, size int64, digest [sha256.Size]byte, content io.Reader) (path string, err error) {
	if err := ValidatePath(root); err != nil {
		return "", err
	}
	if !deliveredName(name) || size < 0 {
		return "", fmt.Errorf("%q cannot be delivered to the Inbox", name)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	staging := filepath.Join(root, ".paperboat-delivery-"+hex.EncodeToString(random[:]))
	file, err := os.OpenFile(staging, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer func() {
		if removeErr := os.Remove(staging); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), content)
	syncErr := file.Sync()
	if closeErr := errors.Join(syncErr, file.Close()); copyErr != nil || closeErr != nil {
		return "", errors.Join(copyErr, closeErr)
	}
	if written != size || !hmacEqual(hash.Sum(nil), digest[:]) {
		return "", ErrDeliveredDigest
	}
	for index := 1; index <= maxDeliveredNames; index++ {
		candidate := filepath.Join(root, numberedName(name, index))
		linkErr := os.Link(staging, candidate)
		if linkErr == nil {
			return candidate, nil
		}
		if !errors.Is(linkErr, os.ErrExist) {
			return "", linkErr
		}
	}
	return "", fmt.Errorf("the Inbox already holds %d files named like %q", maxDeliveredNames, name)
}

func numberedName(name string, index int) string {
	if index <= 1 {
		return name
	}
	extension := filepath.Ext(name)
	return fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, extension), index, extension)
}

func hmacEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var difference byte
	for i := range a {
		difference |= a[i] ^ b[i]
	}
	return difference == 0
}
