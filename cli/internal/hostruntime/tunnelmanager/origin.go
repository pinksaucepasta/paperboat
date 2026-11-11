package tunnelmanager

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
)

// OriginDialer is the context-aware subset of net.Dialer used by readiness
// probes. It is injectable so route activation tests never need local infra.
type OriginDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

// OriginTLSProvider resolves write-only CA and client-certificate references.
// Returned configurations are cloned before use and must not disable
// verification; only the route's insecure or pinned mode does so.
type OriginTLSProvider interface {
	TLSConfig(context.Context, hoststate.TunnelConfigRoute) (*tls.Config, error)
}

// OriginSecretResolver reads a write-only reference from an OS-protected
// credential store. Implementations must never return the reference or secret
// material in an error. The returned bytes are copied and cleared by the TLS
// provider after parsing.
type OriginSecretResolver interface {
	ResolveOriginSecret(context.Context, string) ([]byte, error)
}

type OriginCredentialStore interface {
	Get(string) (string, error)
}

// CredentialStoreOriginSecretResolver adapts the platform keychain,
// Credential Manager, Secret Service, or protected-file store without making
// the origin runtime depend on a concrete OS backend.
type CredentialStoreOriginSecretResolver struct {
	Store OriginCredentialStore
}

func (r CredentialStoreOriginSecretResolver) ResolveOriginSecret(ctx context.Context, reference string) ([]byte, error) {
	if ctx == nil || r.Store == nil {
		return nil, ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	value, err := r.Store.Get(reference)
	if err != nil {
		return nil, errors.New("origin credential store is unavailable")
	}
	return []byte(value), nil
}

// NewOriginRuntime constructs matching probe and request-forwarding policy so
// readiness cannot pass with different TLS settings from live traffic.
func NewOriginRuntime(secrets OriginSecretResolver) (NetworkOriginProber, *OriginStreamForwarder, error) {
	if secrets == nil {
		return NetworkOriginProber{}, nil, ErrInvalidConfig
	}
	provider := ReferenceOriginTLSProvider{Secrets: secrets}
	transport := &OriginHTTPTransport{TLS: provider}
	prober := NetworkOriginProber{TLS: provider}
	return prober, &OriginStreamForwarder{Transport: transport}, nil
}

// ReferenceOriginTLSProvider resolves custom roots and mTLS identities without
// putting PEM material in route state. A client identity is stored as one
// bounded JSON document with certificate_pem and private_key_pem fields.
type ReferenceOriginTLSProvider struct {
	Secrets OriginSecretResolver
}

const maximumOriginTLSSecretBytes = 1 << 20

type originClientIdentity struct {
	CertificatePEM string `json:"certificate_pem"`
	PrivateKeyPEM  string `json:"private_key_pem"`
}

func (p ReferenceOriginTLSProvider) TLSConfig(ctx context.Context, route hoststate.TunnelConfigRoute) (*tls.Config, error) {
	if ctx == nil || p.Secrets == nil {
		return nil, ErrInvalidConfig
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if route.CAReference != nil {
		payload, err := p.resolve(ctx, *route.CAReference)
		if err != nil {
			return nil, err
		}
		defer clear(payload)
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(payload) {
			return nil, errors.Join(ErrInvalidConfig, errors.New("origin CA reference contains no certificates"))
		}
		config.RootCAs = pool
	}
	if route.MTLSCredentialReference != nil {
		payload, err := p.resolve(ctx, *route.MTLSCredentialReference)
		if err != nil {
			return nil, err
		}
		defer clear(payload)
		var identity originClientIdentity
		decoder := json.NewDecoder(bytes.NewReader(payload))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&identity); err != nil || identity.CertificatePEM == "" || identity.PrivateKeyPEM == "" {
			return nil, errors.Join(ErrInvalidConfig, errors.New("origin mTLS reference is invalid"))
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return nil, errors.Join(ErrInvalidConfig, errors.New("origin mTLS reference is invalid"))
		}
		certificate, err := tls.X509KeyPair([]byte(identity.CertificatePEM), []byte(identity.PrivateKeyPEM))
		identity.CertificatePEM = ""
		identity.PrivateKeyPEM = ""
		if err != nil {
			return nil, errors.Join(ErrInvalidConfig, errors.New("origin mTLS reference is invalid"))
		}
		config.Certificates = []tls.Certificate{certificate}
	}
	return config, nil
}

func (p ReferenceOriginTLSProvider) resolve(ctx context.Context, reference string) ([]byte, error) {
	payload, err := p.Secrets.ResolveOriginSecret(ctx, reference)
	if err != nil {
		clear(payload)
		return nil, errors.Join(ErrOriginUnavailable, errors.New("origin credential reference is unavailable"))
	}
	if len(payload) == 0 || len(payload) > maximumOriginTLSSecretBytes {
		clear(payload)
		return nil, errors.Join(ErrInvalidConfig, errors.New("origin credential reference has invalid size"))
	}
	copy := append([]byte(nil), payload...)
	clear(payload)
	return copy, nil
}

type NetworkOriginProber struct {
	Dialer OriginDialer
	TLS    OriginTLSProvider
}

type OriginProbeObservation struct {
	RouteID string
	Code    string
}

// OriginRedirectLoop: live traffic saw the origin send a visitor back to the URL it asked for (the
// edge shows the loop page). It is also the origin-health code the control plane accepts.
const OriginRedirectLoop = "redirect_loop"

func (p NetworkOriginProber) ProbeOrigin(ctx context.Context, route hoststate.TunnelConfigRoute) error {
	if ctx == nil || route.DesiredState != "active" || route.ConnectTimeoutMs < 100 {
		return ErrInvalidConfig
	}
	dialer := p.Dialer
	if dialer == nil {
		dialer = &LocalhostDialer{}
	}
	probeCtx, cancel := context.WithTimeout(ctx, time.Duration(route.ConnectTimeoutMs)*time.Millisecond)
	defer cancel()
	network := "tcp"
	switch route.OriginScheme {
	case "http", "h2c", "tcp":
		if route.TLSVerification != "not_applicable" {
			return ErrInvalidConfig
		}
	case "unix":
		if route.TLSVerification != "not_applicable" {
			return ErrInvalidConfig
		}
		network = "unix"
	case "https":
		switch route.TLSVerification {
		case "system", "custom_ca", "insecure", "pinned":
		default:
			return ErrInvalidConfig
		}
	default:
		return ErrInvalidConfig
	}
	connection, err := dialer.DialContext(probeCtx, network, route.OriginAddress)
	if err != nil {
		return errors.Join(ErrOriginUnavailable, err)
	}
	defer connection.Close()
	if err := probeCtx.Err(); err != nil {
		return errors.Join(ErrOriginUnavailable, err)
	}
	if route.OriginScheme != "https" {
		return nil
	}
	config, err := p.originTLSConfig(probeCtx, withPublicServerName(route, route.MatchHostname))
	if err != nil {
		return errors.Join(ErrOriginUnavailable, err)
	}
	tlsConnection := tls.Client(connection, config)
	if err := tlsConnection.HandshakeContext(probeCtx); err != nil {
		return errors.Join(ErrOriginUnavailable, err)
	}
	return nil
}

func (p NetworkOriginProber) originTLSConfig(ctx context.Context, route hoststate.TunnelConfigRoute) (*tls.Config, error) {
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	needsProvider := route.CAReference != nil || route.MTLSCredentialReference != nil
	if needsProvider {
		if p.TLS == nil {
			return nil, ErrInvalidConfig
		}
		resolved, err := p.TLS.TLSConfig(ctx, route)
		if err != nil || resolved == nil {
			if err == nil {
				err = ErrInvalidConfig
			}
			return nil, err
		}
		config = resolved.Clone()
		if config.MinVersion == 0 || config.MinVersion < tls.VersionTLS12 {
			config.MinVersion = tls.VersionTLS12
		}
		if config.InsecureSkipVerify {
			return nil, ErrInvalidConfig
		}
		if route.TLSVerification == "custom_ca" && config.RootCAs == nil {
			return nil, ErrInvalidConfig
		}
		if route.MTLSCredentialReference != nil && len(config.Certificates) == 0 && config.GetClientCertificate == nil {
			return nil, ErrInvalidConfig
		}
	}
	if route.TLSVerification == "custom_ca" && route.CAReference == nil {
		return nil, ErrInvalidConfig
	}
	if route.TLSVerification != "custom_ca" && route.CAReference != nil {
		return nil, ErrInvalidConfig
	}
	switch route.TLSVerification {
	case "insecure":
		config.InsecureSkipVerify = true //nolint:gosec // the owner turned verification off for this route
	case "pinned":
		if route.TLSPin == nil {
			return nil, ErrInvalidConfig
		}
		pin := *route.TLSPin
		config.InsecureSkipVerify = true //nolint:gosec // VerifyConnection checks the pinned certificate instead of a CA chain
		config.VerifyConnection = func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return &CertificateChangedError{Pinned: pin}
			}
			if presented := CertificateFingerprint(state.PeerCertificates[0]); presented != pin {
				return &CertificateChangedError{Pinned: pin, Presented: presented}
			}
			return nil
		}
	}
	if route.TLSServerName != nil {
		config.ServerName = *route.TLSServerName
	} else if host, _, err := net.SplitHostPort(route.OriginAddress); err == nil {
		config.ServerName = strings.Trim(host, "[]")
	}
	return config, nil
}

var _ OriginProber = NetworkOriginProber{}

// CertificateFingerprint is a pinned route's pin: "sha256:" and the hex SHA-256 of the certificate DER,
// the value browsers and `openssl x509 -fingerprint -sha256` show (without colons, lowercase).
func CertificateFingerprint(certificate *x509.Certificate) string {
	sum := sha256.Sum256(certificate.Raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// CertificateChangedError: a pinned origin presented a different certificate. Known-hosts style, it is
// refused until the owner re-pins the route.
type CertificateChangedError struct{ Pinned, Presented string }

func (e *CertificateChangedError) Error() string {
	return "the origin's certificate changed (pinned " + e.Pinned + ", now " + e.Presented + "); if you replaced it, re-pin the route with `pb tunnel route update <tunnel> <route> --tls-verification pinned`"
}
