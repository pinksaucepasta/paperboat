package derpquic

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"github.com/quic-go/quic-go"
	"net/netip"
	"testing"
	"time"
)

type connectCloser interface {
	Connect(context.Context) error
	Close() error
}

func TestCarrierConnectAdmissionHonorsWaitingContextAndClose(t *testing.T) {
	for _, test := range []struct {
		name string
		new  func(func(context.Context) (string, error)) connectCloser
	}{
		{name: "quic", new: func(credential func(context.Context) (string, error)) connectCloser {
			return NewClient(ClientConfig{Address: "127.0.0.1:1", TLS: &tls.Config{Certificates: []tls.Certificate{{}}}, Credential: credential})
		}},
		{name: "wss", new: func(credential func(context.Context) (string, error)) connectCloser {
			return NewWSSClient(WSSClientConfig{URL: "wss://127.0.0.1:1", TLS: &tls.Config{Certificates: []tls.Certificate{{}}}, Credential: credential})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			started := make(chan struct{})
			carrier := test.new(func(ctx context.Context) (string, error) {
				select {
				case <-started:
				default:
					close(started)
				}
				<-ctx.Done()
				return "", ctx.Err()
			})
			first := make(chan error, 1)
			go func() { first <- carrier.Connect(context.Background()) }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("first connect did not enter credential lookup")
			}
			begin := time.Now()
			if _, err := carrier.(interface {
				LocalAddr() (netip.AddrPort, error)
			}).LocalAddr(); !errors.Is(err, ErrClosed) {
				t.Fatalf("unconnected LocalAddr error=%v", err)
			}
			if time.Since(begin) >= time.Second {
				t.Fatal("LocalAddr waited behind connection setup")
			}

			waiting, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			begin = time.Now()
			if err := carrier.Connect(waiting); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("waiting connect error=%v", err)
			}
			if time.Since(begin) >= time.Second {
				t.Fatal("waiting connect ignored its context")
			}

			closed := make(chan error, 1)
			go func() { closed <- carrier.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("Close did not cancel admitted connect")
			}
			select {
			case <-first:
			case <-time.After(time.Second):
				t.Fatal("admitted connect remained blocked after Close")
			}
		})
	}
}

func TestCarrierCredentialExpiryDoesNotLatchPermanentDenial(t *testing.T) {
	for _, transport := range []string{"quic", "wss"} {
		t.Run(transport, func(t *testing.T) {
			calls := 0
			credential := func(context.Context) (string, error) {
				calls++
				if calls == 1 {
					return "", ErrExpired
				}
				return "", ErrAdmission
			}
			tlsConfig := &tls.Config{Certificates: []tls.Certificate{{}}}
			var c connectCloser
			if transport == "quic" {
				c = NewClient(ClientConfig{Address: "127.0.0.1:1", TLS: tlsConfig, Credential: credential})
			} else {
				c = NewWSSClient(WSSClientConfig{URL: "wss://127.0.0.1:1", TLS: tlsConfig, Credential: credential})
			}
			defer c.Close()
			if err := c.Connect(t.Context()); !errors.Is(err, ErrExpired) {
				t.Fatalf("expiry=%v", err)
			}
			if err := c.Connect(t.Context()); !errors.Is(err, ErrAdmission) || calls != 2 {
				t.Fatalf("fresh lookup denial=%v calls=%d", err, calls)
			}
			if err := c.Connect(t.Context()); !errors.Is(err, ErrAdmission) || calls != 2 {
				t.Fatalf("permanent denial was retried: %v calls=%d", err, calls)
			}
		})
	}
}

func TestCarrierClassificationRetainsPrivateTypedCause(t *testing.T) {
	app := &quic.ApplicationError{ErrorCode: 1, ErrorMessage: "PRIVATE_REMOTE_REASON"}
	err := classify(app)
	var original *quic.ApplicationError
	if !errors.Is(err, ErrAdmission) || !errors.As(err, &original) || original != app || !fatalCarrier(err) || err.Error() != ErrAdmission.Error() {
		t.Fatal("application cause or finite decision lost")
	}
	verification := &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}
	for _, classify := range []func(error) error{classify, classifyWSS} {
		err := classify(verification)
		var original *tls.CertificateVerificationError
		if !errors.Is(err, ErrAdmission) || !errors.As(err, &original) || original != verification || !fatalCarrier(err) || err.Error() != ErrAdmission.Error() {
			t.Fatal("TLS cause or finite decision lost")
		}
	}
}
