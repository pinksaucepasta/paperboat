package protocol

import (
	"errors"
	"sort"
	"strconv"
	"strings"
)

var ErrInvalidCapabilities = errors.New("invalid available capabilities")

type CapabilityProvider interface{ Capabilities() []string }

func AvailableCapabilities(providers ...CapabilityProvider) (map[string]bool, error) {
	available := make(map[string]bool)
	for _, provider := range providers {
		if provider == nil {
			return nil, ErrInvalidCapabilities
		}
		for _, capability := range provider.Capabilities() {
			if !capabilityPattern.MatchString(capability) || available[capability] {
				return nil, ErrInvalidCapabilities
			}
			available[capability] = true
		}
	}
	if !available["terminal.v1"] || !available["health.v1"] {
		return nil, ErrInvalidCapabilities
	}
	return available, nil
}

// ProtocolVersion is the highest version this build speaks and MinProtocolVersion the lowest.
const ProtocolVersion = "1.0"
const MinProtocolVersion = "1.0"

const ProtocolIncompatible Code = "protocol_incompatible"

var requiredCapabilities = map[string]bool{"terminal.v1": true, "health.v1": true}

var allowedCapabilities = map[string]bool{
	"terminal.v1": true, "health.v1": true, "exec.v1": true, "ssh.v1": true,
	"config.compare.v1": true, "config.apply.v1": true, "update.tuf.v1": true, "file-transfer.v1": true, "inspector.v1": true,
	"recording.v1": true,
}

type Negotiator struct {
	Available        map[string]bool
	ConfigApplyProof bool
}

type Welcome struct {
	Version      string
	Capabilities []string
}

// CompatibleVersion accepts any well-formed frame version stamp. The hello exchange (Negotiate) is the
// protocol gate; a newer peer stamps its own build's version, and refusing it would cut released
// peers off from every later release.
func CompatibleVersion(version string) bool {
	_, ok := versionKey(version)
	return ok
}

// Negotiate answers the highest version inside both the peer's [minVersion, maxVersion] and this
// build's [MinProtocolVersion, ProtocolVersion].
func (n Negotiator) Negotiate(minVersion, maxVersion string, offered []string) (Welcome, error) {
	peerMin, okMin := versionKey(minVersion)
	peerMax, okMax := versionKey(maxVersion)
	ownMin, _ := versionKey(MinProtocolVersion)
	ownMax, _ := versionKey(ProtocolVersion)
	if !okMin || !okMax || peerMin > peerMax || peerMax < ownMin || peerMin > ownMax {
		return Welcome{}, &Error{Code: ProtocolIncompatible}
	}
	version := ProtocolVersion
	if peerMax < ownMax {
		version = maxVersion
	}
	offeredSet := make(map[string]bool, len(offered))
	for _, capability := range offered {
		offeredSet[capability] = true
	}
	for capability := range requiredCapabilities {
		if !offeredSet[capability] || !n.Available[capability] {
			return Welcome{}, &Error{Code: CapabilityRequired}
		}
	}
	selected := make([]string, 0, len(offered))
	for capability := range offeredSet {
		if !allowedCapabilities[capability] || !n.Available[capability] {
			continue
		}
		if capability == "config.apply.v1" && !n.ConfigApplyProof {
			continue
		}
		selected = append(selected, capability)
	}
	sort.Strings(selected)
	return Welcome{Version: version, Capabilities: selected}, nil
}

// versionKey orders canonical "major.minor" versions; components are capped so the key cannot overflow.
func versionKey(version string) (uint64, bool) {
	majorText, minorText, ok := strings.Cut(version, ".")
	major, errMajor := strconv.Atoi(majorText)
	minor, errMinor := strconv.Atoi(minorText)
	if !ok || errMajor != nil || errMinor != nil || strconv.Itoa(major) != majorText || strconv.Itoa(minor) != minorText ||
		major < 0 || minor < 0 || major > 1<<20 || minor > 1<<20 {
		return 0, false
	}
	return uint64(major)<<32 | uint64(minor), true
}
