package protocol

import (
	"errors"
	"slices"
	"testing"
)

func TestMachineNegotiationFiltersUnavailableCapabilities(t *testing.T) {
	available := map[string]bool{"terminal.v1": true, "health.v1": true, "exec.v1": true, "ssh.v1": true}
	w, err := (Negotiator{Available: available}).Negotiate("1.0", "1.0", []string{"terminal.v1", "health.v1", "exec.v1", "ssh.v1", "config.apply.v1", "future.v1"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.Capabilities, []string{"exec.v1", "health.v1", "ssh.v1", "terminal.v1"}) {
		t.Fatalf("capabilities=%v", w.Capabilities)
	}
}

func TestNegotiationRequiresVersionAndRequiredCapabilities(t *testing.T) {
	n := Negotiator{Available: map[string]bool{"terminal.v1": true, "health.v1": true}}
	for _, tc := range []struct {
		min, max string
		offered  []string
		code     Code
	}{{"0.1", "0.2", []string{"terminal.v1", "health.v1"}, ProtocolIncompatible}, {"1.0", "1.0", []string{"health.v1"}, CapabilityRequired}} {
		_, err := n.Negotiate(tc.min, tc.max, tc.offered)
		var pe *Error
		if !errors.As(err, &pe) || pe.Code != tc.code {
			t.Fatalf("err=%v want=%s", err, tc.code)
		}
	}
	if _, err := n.Negotiate("1.0", "1.1", []string{"terminal.v1", "health.v1"}); err != nil {
		t.Fatalf("compatible minor range: %v", err)
	}
}

type capabilityProvider []string

func (p capabilityProvider) Capabilities() []string { return append([]string(nil), p...) }

func TestAvailableCapabilitiesDeriveFromImplementedProviders(t *testing.T) {
	available, err := AvailableCapabilities(capabilityProvider{"terminal.v1", "health.v1"}, capabilityProvider{"config.apply.v1"})
	if err != nil || !available["config.apply.v1"] || available["preview.public.v1"] {
		t.Fatalf("available=%v err=%v", available, err)
	}
	for _, providers := range [][]CapabilityProvider{{capabilityProvider{"terminal.v1"}}, {capabilityProvider{"terminal.v1", "health.v1"}, capabilityProvider{"health.v1"}}, {nil}} {
		if _, err := AvailableCapabilities(providers...); !errors.Is(err, ErrInvalidCapabilities) {
			t.Fatalf("providers=%v err=%v", providers, err)
		}
	}
}

func TestMachineConfigApplyCapabilityAlwaysRequiresProof(t *testing.T) {
	available := map[string]bool{"terminal.v1": true, "health.v1": true, "config.apply.v1": true, "update.tuf.v1": true}
	offered := []string{"terminal.v1", "health.v1", "config.apply.v1", "update.tuf.v1"}
	for _, proof := range []bool{false, true} {
		welcome, err := (Negotiator{Available: available, ConfigApplyProof: proof}).Negotiate("1.0", "1.0", offered)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(welcome.Capabilities, "config.apply.v1") != proof {
			t.Fatalf("proof=%v capabilities=%v", proof, welcome.Capabilities)
		}
		if !slices.Contains(welcome.Capabilities, "update.tuf.v1") {
			t.Fatal("proof unexpectedly gates independent machine capability")
		}
	}
}

func TestNegotiatorSelectsFileTransferOnlyWhenImplemented(t *testing.T) {
	offered := []string{"terminal.v1", "health.v1", "file-transfer.v1"}
	for _, enabled := range []bool{false, true} {
		available := map[string]bool{"terminal.v1": true, "health.v1": true, "file-transfer.v1": enabled}
		welcome, err := (Negotiator{Available: available}).Negotiate("1.0", "1.0", offered)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, capability := range welcome.Capabilities {
			if capability == "file-transfer.v1" {
				found = true
			}
		}
		if found != enabled {
			t.Fatalf("implemented=%v selected=%v", enabled, found)
		}
	}
}
