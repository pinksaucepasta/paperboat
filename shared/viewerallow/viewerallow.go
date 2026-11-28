// Package viewerallow is the syntax and matching of a browser viewer allow list: exact email addresses and
// "*@domain" entries. pb checks a list with it before it creates anything, and the server stores and matches with it.
// Preview, tunnel and terminal sharing all use it; terminal sharing uses the Terminal rules, which keep the terminal
// API's older, more lenient input handling (see Rules).
package viewerallow

import (
	"errors"
	"fmt"
	"net/mail"
	"net/netip"
	"slices"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// MaxEntries bounds a list (the database holds the same bound).
const MaxEntries = 100

const maxEntryBytes = 254

const wildcardPrefix = "*@"

var ErrInvalid = errors.New("invalid viewer allow list")

// Rules are the variations between the two users of one syntax. Everything else is shared.
type Rules struct {
	// fold trims surrounding space and lower-cases an entry before checking it.
	fold bool
	// unicodeLocal admits non-ASCII characters in an address's local part.
	unicodeLocal bool
	// ipDomain admits a domain that is an IP address in dotted form.
	ipDomain bool
	// wildcardUnbounded skips the whole-entry byte bound on "*@domain" (the domain keeps its own bound).
	wildcardUnbounded bool
	// listSortedMayBeEmpty admits an empty list, and Canonical sorts the result.
	listSortedMayBeEmpty bool
}

var (
	// Viewer is the rule set for preview and tunnel allow lists.
	Viewer = Rules{}
	// Terminal is the rule set for terminal sharing: entries are trimmed and lower-cased, a local part may be
	// non-ASCII, an IP-form domain is admitted, and a list may be empty and is stored sorted.
	Terminal = Rules{fold: true, unicodeLocal: true, ipDomain: true, wildcardUnbounded: true, listSortedMayBeEmpty: true}
)

// Email returns the canonical form of one exact address by the Viewer rules.
func Email(raw string) (string, bool) { return Viewer.Email(raw) }

// Domain returns the canonical ASCII form of an email domain by the Viewer rules.
func Domain(raw string) (string, bool) { return Viewer.Domain(raw) }

// Entry returns the canonical form of one list entry by the Viewer rules.
func Entry(raw string) (string, bool) { return Viewer.Entry(raw) }

// Canonical validates a whole list by the Viewer rules.
func Canonical(raw []string) ([]string, error) { return Viewer.Canonical(raw) }

// Allows reports whether the canonical list admits the address by the Viewer rules.
func Allows(list []string, address string) bool { return Viewer.Allows(list, address) }

func (r Rules) prepare(raw string) string {
	if r.fold {
		return strings.ToLower(strings.TrimSpace(raw))
	}
	return raw
}

// Email returns the canonical form of one exact address: lower case, with an ASCII (IDNA) domain of at least two
// labels.
func (r Rules) Email(raw string) (string, bool) {
	raw = r.prepare(raw)
	if len(raw) == 0 || len(raw) > maxEntryBytes || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "*?") || !utf8.ValidString(raw) {
		return "", false
	}
	for _, c := range raw {
		if c <= 0x20 || c == 0x7f {
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
	if !r.unicodeLocal {
		for _, c := range local {
			if c > 0x7f {
				return "", false
			}
		}
	}
	canonical, ok := r.domain(domain)
	if !ok || !strings.Contains(canonical, ".") {
		return "", false
	}
	return strings.ToLower(local) + "@" + canonical, true
}

// Domain returns the canonical ASCII form of an email domain.
func (r Rules) Domain(raw string) (string, bool) { return r.domain(r.prepare(raw)) }

func (r Rules) domain(raw string) (string, bool) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "*/\\:@%?#\r\n\t ") || strings.HasSuffix(raw, ".") || !utf8.ValidString(raw) {
		return "", false
	}
	for _, c := range raw {
		if c == 0x7f || c < 0x20 {
			return "", false
		}
	}
	ascii, err := idna.Lookup.ToASCII(strings.ToLower(raw))
	if err != nil || ascii == "" || len(ascii) > 253 {
		return "", false
	}
	ascii = strings.ToLower(ascii)
	if _, err := netip.ParseAddr(ascii); err == nil && !r.ipDomain {
		return "", false
	}
	for _, label := range strings.Split(ascii, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return "", false
			}
		}
	}
	return ascii, true
}

// Entry returns the canonical form of one list entry: an exact address, or "*@domain" for every address at exactly
// that domain. A wildcard's domain has at least two labels, so there is no "*", "*@*" or "*@com".
func (r Rules) Entry(raw string) (string, bool) {
	raw = r.prepare(raw)
	rest, wildcard := strings.CutPrefix(raw, wildcardPrefix)
	if !wildcard {
		return r.Email(raw)
	}
	if len(raw) > maxEntryBytes && !r.wildcardUnbounded {
		return "", false
	}
	domain, ok := r.domain(rest)
	// The mapped form is checked, not the typed one: IDNA turns other dot characters into ".", and may end in one.
	if !ok || !strings.Contains(domain, ".") || len(wildcardPrefix)+len(domain) > maxEntryBytes && !r.wildcardUnbounded {
		return "", false
	}
	return wildcardPrefix + domain, true
}

// Canonical validates a whole list: at most MaxEntries entries (at least one, unless the rules admit an empty list),
// no entry twice.
func (r Rules) Canonical(raw []string) ([]string, error) {
	if len(raw) == 0 && !r.listSortedMayBeEmpty {
		return nil, fmt.Errorf("%w: list at least one email address or *@domain", ErrInvalid)
	}
	if len(raw) > MaxEntries {
		return nil, fmt.Errorf("%w: at most %d entries", ErrInvalid, MaxEntries)
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, value := range raw {
		entry, ok := r.Entry(value)
		if !ok {
			return nil, fmt.Errorf("%w: %q is not an email address or *@domain", ErrInvalid, value)
		}
		if _, exists := seen[entry]; exists {
			return nil, fmt.Errorf("%w: %q is listed twice", ErrInvalid, value)
		}
		seen[entry] = struct{}{}
		out = append(out, entry)
	}
	if r.listSortedMayBeEmpty {
		slices.Sort(out)
	}
	return out, nil
}

// Emails validates a list of exact addresses only (no "*@domain"), with the same bounds as Canonical.
func (r Rules) Emails(raw []string) ([]string, error) {
	for _, value := range raw {
		if _, wildcard := strings.CutPrefix(r.prepare(value), wildcardPrefix); wildcard {
			return nil, fmt.Errorf("%w: %q is not an email address", ErrInvalid, value)
		}
	}
	return r.Canonical(raw)
}

// Allows reports whether the canonical list admits the address. A "*@domain" entry admits addresses at exactly that
// domain, never at a subdomain or at a name that only ends with it. An address that is not valid is never admitted.
// The caller must have established that the address is verified.
func (r Rules) Allows(list []string, address string) bool {
	canonical, ok := r.Email(address)
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
