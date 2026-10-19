package connectorprotocol

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Capability names are stable wire names. The required set is intentionally
// small so optional data-plane features can be negotiated independently.
const (
	CapabilitySnapshot           = "config.snapshot.v1"
	CapabilityDelta              = "config.delta.v1"
	CapabilityAck                = "config.ack.v1"
	CapabilityHeartbeat          = "session.heartbeat.v1"
	CapabilityRenewal            = "auth.renew.v1"
	CapabilityDrain              = "connector.drain.v1"
	CapabilityCredentialRotation = "credential.rotate.v1"
)

var requiredCapabilities = map[string]struct{}{
	CapabilitySnapshot: {}, CapabilityDelta: {}, CapabilityAck: {},
	CapabilityHeartbeat: {}, CapabilityRenewal: {},
}

// ProductionCapabilities returns a fresh copy of every connector-v1
// capability implemented by the canonical server and stable host runtime.
func ProductionCapabilities() []string {
	return []string{
		CapabilitySnapshot,
		CapabilityDelta,
		CapabilityAck,
		CapabilityHeartbeat,
		CapabilityRenewal,
		CapabilityDrain,
		CapabilityCredentialRotation,
	}
}

func ValidateCapabilities(values []string) error {
	if len(values) == 0 || len(values) > MaxCapabilities {
		return ErrCapabilityMissing
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if len(value) > MaxCapabilityBytes || !capabilityPattern.MatchString(value) {
			return ErrInvalidInput
		}
		if _, exists := seen[value]; exists {
			return ErrInvalidInput
		}
		seen[value] = struct{}{}
	}
	for required := range requiredCapabilities {
		if _, ok := seen[required]; !ok {
			return codeError(ErrCapabilityMissing, ReasonCapabilityMissing, false, fmt.Errorf("missing capability %q", required))
		}
	}
	return nil
}

func NegotiateCapabilities(offered, supported []string) ([]string, error) {
	if err := ValidateCapabilities(offered); err != nil {
		return nil, err
	}
	if err := ValidateCapabilities(supported); err != nil {
		return nil, err
	}
	supportedSet := make(map[string]struct{}, len(supported))
	for _, value := range supported {
		supportedSet[value] = struct{}{}
	}
	selected := make([]string, 0, len(offered))
	for _, value := range offered {
		if _, ok := supportedSet[value]; ok {
			selected = append(selected, value)
		}
	}
	if err := ValidateCapabilities(selected); err != nil {
		return nil, codeError(ErrCapabilityMissing, ReasonCapabilityMissing, false, err)
	}
	return selected, nil
}

func NegotiateVersion(minVersion, maxVersion string) (string, error) {
	if !versionPattern.MatchString(minVersion) || !versionPattern.MatchString(maxVersion) {
		return "", codeError(ErrProtocolIncompatible, ReasonProtocolMismatch, false, errors.New("invalid version range"))
	}
	minMajor, minMinor, minOK := parseVersion(minVersion)
	maxMajor, maxMinor, maxOK := parseVersion(maxVersion)
	selectedMajor, selectedMinor, selectedOK := parseVersion(ProtocolVersion)
	if !maxOK || !selectedOK {
		return "", codeError(ErrProtocolIncompatible, ReasonProtocolMismatch, false, errors.New("invalid protocol version"))
	}
	if !minOK {
		return "", codeError(ErrProtocolIncompatible, ReasonProtocolMismatch, false, errors.New("invalid protocol version"))
	}
	if minMajor > maxMajor || minMajor > selectedMajor || maxMajor < selectedMajor || minMajor == selectedMajor && minMinor > selectedMinor || maxMajor == selectedMajor && maxMinor < selectedMinor {
		return "", codeError(ErrProtocolIncompatible, ReasonProtocolMismatch, false, errors.New("no compatible protocol version"))
	}
	return ProtocolVersion, nil
}

func parseVersion(value string) (int, int, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return 0, 0, false
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	if errMajor != nil || errMinor != nil || major < 0 || minor < 0 || strconv.Itoa(major) != parts[0] || strconv.Itoa(minor) != parts[1] {
		return 0, 0, false
	}
	return major, minor, true
}
