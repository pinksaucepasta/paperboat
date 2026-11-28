// Package viewerallow is the syntax and matching of a browser viewer allow list: exact email addresses and
// "*@domain" entries. pb checks a list with it before it creates anything, and the server stores and matches with it.
package viewerallow

import (
	"errors"
	"fmt"
	"net/mail"
	"net/netip"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// MaxEntries bounds a list (the database holds the same bound).
const MaxEntries = 100

const maxEntryBytes = 254

const wildcardPrefix = "*@"

var ErrInvalid = errors.New("invalid viewer allow list")

// Email returns the canonical form of one exact address: lower case, with an ASCII (IDNA) domain.
func Email(raw string) (string, bool) {
	if len(raw) == 0 || len(raw) > maxEntryBytes || strings.TrimSpace(raw) != raw || strings.Contains(raw, "*") || !utf8.ValidString(raw) {
		return "", false
	}
	for _, r := range raw {
		if r <= 0x20 || r == 0x7f {
			return "", false
		}
	}
	parsed, err := mail.ParseAddress(raw)
	if err != nil || parsed.Address != raw || strings.Count(raw, "@") != 1 {
		return "", false
	}
	local, domain, found := strings.Cut(raw, "@")
	if !found || local == "" {
		return "", false
	}
	for _, r := range local {
		if r > 0x7f {
			return "", false
		}
	}
	canonical, ok := Domain(domain)
	if !ok {
		return "", false
	}
	return strings.ToLower(local) + "@" + canonical, true
}

// Domain returns the canonical ASCII form of an email domain.
func Domain(raw string) (string, bool) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "*/\\:@%?#\r\n\t ") || strings.HasSuffix(raw, ".") || !utf8.ValidString(raw) {
		return "", false
	}
	for _, r := range raw {
		if r == 0x7f || r < 0x20 {
			return "", false
		}
	}
	lower := strings.ToLower(raw)
	ascii, err := idna.Lookup.ToASCII(lower)
	if err != nil || ascii == "" || len(ascii) > 253 || strings.Contains(ascii, "..") {
		return "", false
	}
	ascii = strings.ToLower(ascii)
	if _, err := netip.ParseAddr(ascii); err == nil {
		return "", false
	}
	for _, label := range strings.Split(ascii, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
	}
	return ascii, true
}

// Entry returns the canonical form of one list entry: an exact address, or "*@domain" for every address at exactly
// that domain. A wildcard's domain has at least two labels, so there is no "*", "*@*" or "*@com".
func Entry(raw string) (string, bool) {
	rest, wildcard := strings.CutPrefix(raw, wildcardPrefix)
	if !wildcard {
		return Email(raw)
	}
	if len(raw) > maxEntryBytes {
		return "", false
	}
	domain, ok := Domain(rest)
	// The mapped form is checked, not the typed one: IDNA turns other dot characters into ".", and may end in one.
	if !ok || !strings.Contains(domain, ".") || len(wildcardPrefix)+len(domain) > maxEntryBytes {
		return "", false
	}
	return wildcardPrefix + domain, true
}

// Canonical validates a whole list: 1 to MaxEntries entries, no entry twice.
func Canonical(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: list at least one email address or *@domain", ErrInvalid)
	}
	if len(raw) > MaxEntries {
		return nil, fmt.Errorf("%w: at most %d entries", ErrInvalid, MaxEntries)
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, value := range raw {
		entry, ok := Entry(value)
		if !ok {
			return nil, fmt.Errorf("%w: %q is not an email address or *@domain", ErrInvalid, value)
		}
		if _, exists := seen[entry]; exists {
			return nil, fmt.Errorf("%w: %q is listed twice", ErrInvalid, value)
		}
		seen[entry] = struct{}{}
		out = append(out, entry)
	}
	return out, nil
}

// Allows reports whether the canonical list admits the address. A "*@domain" entry admits addresses at exactly that
// domain, never at a subdomain or at a name that only ends with it. An address that is not valid is never admitted.
// The caller must have established that the address is verified.
func Allows(list []string, address string) bool {
	canonical, ok := Email(address)
	if !ok {
		return false
	}
	_, domain, _ := strings.Cut(canonical, "@")
	for _, entry := range list {
		if entry == canonical || entry == wildcardPrefix+domain {
			return true
		}
	}
	return false
}
