package tunnelenrollment

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"testing"

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
