package splitdns

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/netip"
	"strconv"
	"strings"
)

// BrowserRoute binds one flat browser site to an explicitly authorized service.
type BrowserRoute struct {
	Address netip.Addr
	Port    int
}

// BrowserHostname separates cookies and schemeful sites for each machine/port.
// The validated private suffix is always a single label; nesting below a device
// hostname would put unrelated applications in the same registrable domain.
func BrowserHostname(machineID string, port int, suffix string) (string, error) {
	if _, err := ValidateSuffix(suffix); err != nil {
		return "", err
	}
	if machineID == "" || port < 1 || port > 65535 {
		return "", errors.New("invalid browser service identity")
	}
	digest := sha256.Sum256([]byte(machineID + "\x00" + strconv.Itoa(port)))
	return hex.EncodeToString(digest[:16]) + "." + suffix, nil
}

func validBrowserHost(host, suffix string) bool {
	labels := strings.Split(host, ".")
	if len(labels) != 2 || labels[1] != suffix || len(labels[0]) < 1 || len(labels[0]) > 63 || strings.HasPrefix(labels[0], "-") || strings.HasSuffix(labels[0], "-") {
		return false
	}
	for _, r := range labels[0] {
		if r != '-' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
