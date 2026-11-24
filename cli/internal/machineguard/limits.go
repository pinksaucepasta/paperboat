package machineguard

import (
	"errors"
	"fmt"
	"io"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

// Every size the machine guard accepts follows from MaxRoutedMachines. The guard is root and parses requests of any
// local OS user, so each bound is strict; the coordinator trims its requests to what fits (and pb doctor says so),
// so an account above a bound keeps everything below it. Prior art: tailscale control/controlclient/direct.go
// (maxCompressedMapResponseSize, checked before allocating), netbird client/internal/daemonaddr/grpc.go (16 MiB).
const (
	// MaxRoutedMachines is how many machines one daemon serves locally: what one peer configuration can name.
	MaxRoutedMachines = peernetwork.MaxEndpoints
	// MaxMachineListeners bounds the machine ports one daemon listens for: four per machine.
	MaxMachineListeners = 4 * MaxRoutedMachines
	// MaxBrowserAliases and MaxBrowserAliasBytes (names and targets) bound the browser names of one connection.
	MaxBrowserAliases    = 4 * MaxRoutedMachines
	MaxBrowserAliasBytes = maxFrame / 2

	// maxFrame bounds one request or response on the control connection.
	maxFrame = 2 << 20
	// maxUserConnections bounds the control connections of one OS user.
	maxUserConnections = 4
	// maxUserListeners adds the browser gateway's own ports to one user's machine listeners.
	maxUserListeners = MaxMachineListeners + MaxRoutedMachines
	maxHostListeners = 4 * maxUserListeners
	// maxUserNames: every machine name and browser alias of one user.
	maxUserNames = MaxRoutedMachines + MaxBrowserAliases
	maxHostNames = 4 * maxUserNames
	// maxUserClaims leaves room for the machines whose addresses are still held (claimHold) after they left.
	maxUserClaims = 2 * MaxRoutedMachines
	maxHostClaims = 65533
	// maxClaimMachines bounds the inventory of one "claims" request, offline machines included.
	maxClaimMachines = 4 * MaxRoutedMachines
	// maxClaimField bounds the account and machine ids of a claim.
	maxClaimField = 128
	// maxCachedCertificates bounds the leaf keys kept per connection; above it a certificate is issued per request.
	maxCachedCertificates = MaxRoutedMachines
	// maxJournal bounds reservations.json. It is above the encoded size of maxHostNames and maxHostClaims full entries.
	maxJournal = 64 << 20
	// maxStoredLeaseNames bounds the descriptor names kept in systemd's store: the next guard gets them in one
	// environment string (LISTEN_FDNAMES), and exec refuses one above 128 KiB.
	maxStoredLeaseNames = 96 << 10
	// maxRefusedNamesListed bounds the names one refusal spells out.
	maxRefusedNamesListed = 20
)

var errFrameSize = errors.New("invalid machine guard frame size")

// checkFrame refuses a frame before any of it is written, so the connection (and every lease on it) stays usable.
func checkFrame(data []byte) error {
	if len(data) == 0 || len(data) > maxFrame {
		return fmt.Errorf("%w: %d bytes, limit %d", errFrameSize, len(data), maxFrame)
	}
	return nil
}

// readFrameBody reads size bytes, growing with what really arrives: a header alone reserves no memory.
func readFrameBody(reader io.Reader, size uint32) ([]byte, error) {
	if size == 0 || size > maxFrame {
		return nil, errFrameSize
	}
	data, err := io.ReadAll(io.LimitReader(reader, int64(size)))
	if err == nil && len(data) != int(size) {
		err = io.ErrUnexpectedEOF
	}
	return data, err
}

// validClaimField admits one opaque id; the characters encode without JSON escapes, so maxJournal holds.
func validClaimField(value string) bool {
	if value == "" || len(value) > maxClaimField {
		return false
	}
	for i := range len(value) {
		c := value[i]
		if c <= ' ' || c > '~' || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			return false
		}
	}
	return true
}
