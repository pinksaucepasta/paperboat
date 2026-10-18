package tunnelenrollment

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/quic-go/quic-go"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
)

func TestCarrierDialDiagnosticUsesFiniteTypedCategories(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("dial: %w", x509.HostnameError{Certificate: &x509.Certificate{}, Host: "private.example"}), "carrier_tls_hostname"},
		{fmt.Errorf("dial: %w", x509.UnknownAuthorityError{Cert: &x509.Certificate{}}), "carrier_tls_root"},
		{x509.CertificateInvalidError{Cert: &x509.Certificate{}, Reason: x509.Expired}, "carrier_tls_expired"},
		{x509.CertificateInvalidError{Cert: &x509.Certificate{}, Reason: x509.IncompatibleUsage}, "carrier_tls_certificate"},
		{context.DeadlineExceeded, "carrier_timeout"},
		{connector.ErrHTTP3Unavailable, "carrier_timeout"},
		{connector.ErrDataCarrierAdmission, "carrier_admission"},
		{connector.ErrInvalidDataCarrierEndpoint, "carrier_endpoint"},
		{connector.ErrDataCarrierTLS, "carrier_tls"},
		{errors.New("secret peer response must never be emitted"), "carrier_transport"},
	} {
		if got := carrierDialDiagnostic(test.err); got != test.want {
			t.Fatalf("category=%q want=%q", got, test.want)
		}
	}
}

func TestQUICErrorDiagnosticNamesSideAndCodeWithoutMessage(t *testing.T) {
	carrier := func(err error) error {
		return &connector.HTTPCarrierFailure{Stage: "round_trip", Cause: errors.Join(connector.ErrDataCarrierTLS, fmt.Errorf("wrap: %w", err))}
	}
	for _, test := range []struct {
		err  error
		want string
	}{
		{carrier(&quic.TransportError{Remote: true, ErrorCode: 0x12a, ErrorMessage: "secret peer text"}), "transport_remote_0x12a"},
		{carrier(&quic.TransportError{ErrorCode: quic.ConnectionRefused}), "transport_local_0x2"},
		{carrier(&quic.ApplicationError{Remote: true, ErrorCode: 0x100, ErrorMessage: "secret"}), "application_remote_0x100"},
		{carrier(&quic.IdleTimeoutError{}), "idle_timeout"},
		{carrier(&quic.HandshakeTimeoutError{}), "handshake_timeout"},
		{carrier(errors.New("not quic")), ""},
	} {
		got := quicErrorDiagnostic(test.err)
		if got != test.want {
			t.Fatalf("quic_error=%q want=%q", got, test.want)
		}
		if got != "" {
			if _, err := diagnostics.NewEvent(time.Now(), "tunnel", "carrier_tls", "warning", map[string]string{"quic_error": got}); err != nil {
				t.Fatalf("diagnostic event rejected %q: %v", got, err)
			}
		}
	}
}
