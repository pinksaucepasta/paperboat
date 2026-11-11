package main

import (
	"context"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

type recordingRegistrar struct {
	requests []api.PeerNetworkRegistration
	failAt   int
}

func (r *recordingRegistrar) RegisterPeerNetwork(_ context.Context, request api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error) {
	r.requests = append(r.requests, request)
	if len(r.requests) == r.failAt {
		return api.PeerNetworkRegistrationResult{}, errors.New("registration unavailable")
	}
	return api.PeerNetworkRegistrationResult{KeyGeneration: request.ExpectedKeyGeneration + 1}, nil
}

func TestRotationRestoresOriginalIdentityAtNewGeneration(t *testing.T) {
	original := api.PeerNetworkRegistration{WireGuardPublicKey: "wireguard", DiscoPublicKey: "original", ExpectedKeyGeneration: 7, QUICCertificateFingerprint: "certificate"}
	r := &recordingRegistrar{}
	raised, restored, err := rotate(r, original)
	if err != nil || raised != 8 || restored != 9 || len(r.requests) != 2 {
		t.Fatalf("rotation receipt: %d %d %v", raised, restored, err)
	}
	first, last := r.requests[0], r.requests[1]
	if first.DiscoPublicKey == original.DiscoPublicKey || first.WireGuardPublicKey != original.WireGuardPublicKey || first.QUICCertificateFingerprint != original.QUICCertificateFingerprint {
		t.Fatal("rotation must change only disco key")
	}
	if last.DiscoPublicKey != original.DiscoPublicKey || last.WireGuardPublicKey != original.WireGuardPublicKey || last.QUICCertificateFingerprint != original.QUICCertificateFingerprint || last.ExpectedKeyGeneration != raised || first.OperationID == last.OperationID {
		t.Fatal("original identity was not restored with fresh operation and generation")
	}
}

func TestFailedRestorationCannotReportRestored(t *testing.T) {
	r := &recordingRegistrar{failAt: 2}
	raised, restored, err := rotate(r, api.PeerNetworkRegistration{ExpectedKeyGeneration: 3})
	if err == nil || raised != 4 || restored != 0 {
		t.Fatalf("failure receipt: %d %d %v", raised, restored, err)
	}
}
