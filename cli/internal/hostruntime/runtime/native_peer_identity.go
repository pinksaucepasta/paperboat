//go:build darwin || linux || windows

package runtime

import (
	"crypto/tls"
	"encoding/base64"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/shared/peertransport/endpointidentity"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

// The primary and delegated carriers use the same enrolled machine identity.
// Loading this material never enrolls or registers a second network endpoint.
func loadNativePeerTLS(stateRoot, machineID string, generation uint64) (*tls.Config, *tls.Config, tailnet.NetworkBinding, error) {
	endpoint, err := runtimeEnvironmentEndpoint(stateRoot)
	if err != nil || endpoint.Generation != generation {
		return nil, nil, tailnet.NetworkBinding{}, errors.Join(ErrProductionInvalid, err)
	}
	if len(endpoint.Certificate) == 0 {
		// Approval is asynchronous. Keep the host runtime alive while the
		// enrollment service polls; no peer listener or relay is exposed until
		// a certificate can be verified for this exact machine generation.
		return nil, nil, tailnet.NetworkBinding{}, errNativePeerPending
	}
	expected := endpointidentity.Expected{Role: endpointidentity.RoleMachine, EndpointID: machineID, Generation: endpoint.Generation}
	certificate, err := endpointidentity.Verify(endpoint.Certificate, endpoint.RootPublicKey, expected, serverclock.Now().UTC())
	if err != nil {
		// This machine's own certificate, now expired, waits like a missing one: enrollment renews it, and a removed
		// machine, which gets no new one, keeps its carrier and so can be reinstalled. Anything else stays invalid.
		if stored, parseErr := endpointidentity.Parse(endpoint.Certificate); parseErr == nil && serverclock.Now().After(stored.Claims.ExpiresAt) {
			if _, ownErr := endpointidentity.Verify(endpoint.Certificate, endpoint.RootPublicKey, expected, stored.Claims.IssuedAt); ownErr == nil {
				return nil, nil, tailnet.NetworkBinding{}, errNativePeerPending
			}
		}
		return nil, nil, tailnet.NetworkBinding{}, errors.Join(ErrProductionInvalid, err)
	}
	issueLeaf := func() (tls.Certificate, error) {
		now := serverclock.Now().UTC()
		lifetime := certificate.Claims.ExpiresAt.Sub(now)
		if lifetime > 24*time.Hour {
			lifetime = 24 * time.Hour
		}
		return endpointidentity.NewTLSCertificate(certificate, endpoint.RootPublicKey, endpoint.QUICPrivateKey, now, lifetime)
	}
	leaf, err := issueLeaf()
	if err != nil {
		return nil, nil, tailnet.NetworkBinding{}, errors.Join(ErrProductionInvalid, err)
	}
	getCertificate := func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		fresh, issueErr := issueLeaf()
		return &fresh, issueErr
	}
	getClientCertificate := func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		fresh, issueErr := issueLeaf()
		return &fresh, issueErr
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, InsecureSkipVerify: true, NextProtos: []string{peerquic.ALPN}, Certificates: []tls.Certificate{leaf}, GetCertificate: getCertificate, GetClientCertificate: getClientCertificate} //nolint:gosec
	relayTLS := &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{leaf}, GetClientCertificate: getClientCertificate}
	self := tailnet.NetworkBinding{AccountID: certificate.Claims.AccountID, EndpointID: machineID, Role: "machine", MachineID: machineID, EndpointGeneration: endpoint.Generation, MachineGeneration: generation, QUICCertificateFingerprint: certificate.Fingerprint(), QUICPublicKey: base64.RawURLEncoding.EncodeToString(endpoint.QUICPublicKey())}
	return tlsConfig, relayTLS, self, nil
}
