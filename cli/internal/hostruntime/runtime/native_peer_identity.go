//go:build darwin || linux || windows

package runtime

import (
	"crypto/tls"
	"encoding/base64"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/shared/peertransport/endpointidentity"
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
	certificate, err := endpointidentity.Verify(endpoint.Certificate, endpoint.RootPublicKey, endpointidentity.Expected{Role: endpointidentity.RoleMachine, EndpointID: machineID, Generation: endpoint.Generation}, time.Now().UTC())
	if err != nil {
		return nil, nil, tailnet.NetworkBinding{}, errors.Join(ErrProductionInvalid, err)
	}
	issueLeaf := func() (tls.Certificate, error) {
		now := time.Now().UTC()
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
