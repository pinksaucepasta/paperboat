package machineguard

import (
	"errors"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

// claimHold is how long a released address stays reserved for its previous (uid, account, machine) before
// any other claimant may get it (PLAN 3b multi-account hosts, Lane X 1).
const claimHold = 7 * 24 * time.Hour

const (
	maxClaimsPerUID = 512
	maxHostClaims   = 65533
)

var errClaimsExhausted = listenerRefusal{reason: "no free local machine address on this computer"}

// claimKey names one machine of one account for one OS user incarnation. A legacy entry carried over from the
// host-wide reservations has no account and machine; its address is in legacyIP.
type claimKey struct {
	UID, Account, Machine, legacyIP string
}

func (k claimKey) String() string {
	if k.legacyIP != "" {
		return k.UID + "\x00\x00legacy:" + k.legacyIP
	}
	return k.UID + "\x00" + k.Account + "\x00" + k.Machine
}

func parseClaimKey(value string) (claimKey, bool) {
	parts := strings.Split(value, "\x00")
	if len(parts) != 3 || parts[0] == "" {
		return claimKey{}, false
	}
	if parts[1] == "" && strings.HasPrefix(parts[2], "legacy:") {
		return claimKey{UID: parts[0], legacyIP: strings.TrimPrefix(parts[2], "legacy:")}, true
	}
	return claimKey{UID: parts[0], Account: parts[1], Machine: parts[2]}, parts[1] != "" && parts[2] != ""
}

type claim struct {
	IP string `json:"ip"`
	// HeldUntil is set while the claim is released: the address stays reserved for this key until then.
	HeldUntil time.Time `json:"held_until,omitempty"`
}

// claimBook is the guard's address claims: one address per (uid, account, machine), persisted, so a user keeps
// the same local address for a machine on this host. Claims outlive listeners; they end only when released and
// held for claimHold.
type claimBook struct {
	Claims map[string]claim `json:"claims"`
}

func newClaimBook() *claimBook { return &claimBook{Claims: map[string]claim{}} }

// adoptLegacy carries today's host-wide reservations (address -> uid) over as legacy entries that their uid adopts
// one by one, by asking for that exact address; entries nobody adopts are released after claimHold.
func (b *claimBook) adoptLegacy(ips map[string]string, now time.Time) {
	for ip, uid := range ips {
		if uid == "" {
			continue
		}
		b.Claims[claimKey{UID: uid, legacyIP: ip}.String()] = claim{IP: ip, HeldUntil: now.Add(claimHold)}
	}
}

func (b *claimBook) holder(ip string) (string, bool) {
	for key, value := range b.Claims {
		if value.IP == ip {
			return key, true
		}
	}
	return "", false
}

// assign returns the address of key: its existing claim (reactivated if held), else the legacy entry of the same uid
// at the preferred address, else the preferred address if nobody holds it, else the lowest free address.
func (b *claimBook) assign(key claimKey, preferred netip.Addr, now time.Time) (string, error) {
	b.expire(now)
	if existing, ok := b.Claims[key.String()]; ok {
		existing.HeldUntil = time.Time{}
		b.Claims[key.String()] = existing
		return existing.IP, nil
	}
	owned := 0
	for name := range b.Claims {
		if other, ok := parseClaimKey(name); ok && other.UID == key.UID {
			owned++
		}
	}
	if owned >= maxClaimsPerUID || len(b.Claims) >= maxHostClaims {
		return "", errClaimsExhausted
	}
	want := preferred.String()
	if holder, held := b.holder(want); held {
		if other, ok := parseClaimKey(holder); ok && other.legacyIP == want && other.UID == key.UID {
			delete(b.Claims, holder)
			b.Claims[key.String()] = claim{IP: want}
			return want, nil
		}
	} else if validClaimAddress(preferred) {
		b.Claims[key.String()] = claim{IP: want}
		return want, nil
	}
	used := make(map[string]bool, len(b.Claims))
	for _, value := range b.Claims {
		used[value.IP] = true
	}
	for x := 0; x < 256; x++ {
		for y := 1; y < 255; y++ {
			candidate := netip.AddrFrom4([4]byte{127, 100, byte(x), byte(y)})
			if validClaimAddress(candidate) && !used[candidate.String()] {
				b.Claims[key.String()] = claim{IP: candidate.String()}
				return candidate.String(), nil
			}
		}
	}
	return "", errClaimsExhausted
}

// releaseAbsent holds every active claim of (uid, account) whose machine is not in present. The caller sends
// present only from a complete, verified configuration; an empty set while claims exist is refused.
func (b *claimBook) releaseAbsent(uid, account string, present map[string]bool, now time.Time) error {
	if uid == "" || account == "" {
		return errors.New("claims need an OS user and an account")
	}
	if len(present) == 0 {
		for name, value := range b.Claims {
			if key, ok := parseClaimKey(name); ok && key.UID == uid && key.Account == account && value.HeldUntil.IsZero() {
				return errors.New("an empty machine set would release every claim; refused (offline until revoked)")
			}
		}
	}
	for name, value := range b.Claims {
		key, ok := parseClaimKey(name)
		if !ok || key.UID != uid || key.Account != account || present[key.Machine] || !value.HeldUntil.IsZero() {
			continue
		}
		value.HeldUntil = now.Add(claimHold)
		b.Claims[name] = value
	}
	return nil
}

// expire drops claims whose hold has ended.
func (b *claimBook) expire(now time.Time) {
	for name, value := range b.Claims {
		if !value.HeldUntil.IsZero() && !now.Before(value.HeldUntil) {
			delete(b.Claims, name)
		}
	}
}

// addresses lists the claimed addresses of uid, sorted, for status and tests.
func (b *claimBook) addresses(uid string) []string {
	var out []string
	for name, value := range b.Claims {
		if key, ok := parseClaimKey(name); ok && key.UID == uid {
			out = append(out, value.IP)
		}
	}
	sort.Strings(out)
	return out
}

func validClaimAddress(address netip.Addr) bool {
	if !address.Is4() {
		return false
	}
	b := address.As4()
	return b[0] == 127 && b[1] == 100 && b[3] > 0 && b[3] < 255 && address.String() != splitdns.BrowserGatewayIP
}
