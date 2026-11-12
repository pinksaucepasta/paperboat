package preview

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"
)

// An https preview origin on localhost or an IP literal is sent the preview's public name as SNI (Caddy and Traefik pick the
// site by it); a DNS-name origin keeps its own name. Found on a real device: the raw path sent no SNI at all (F7-08).
func TestPreviewHTTPSOriginGetsThePublicNameAsSNI(t *testing.T) {
	for _, test := range []struct {
		name    string
		address func(port string) string
		public  string
		want    string
	}{
		{"loopback ip", func(port string) string { return "127.0.0.1:" + port }, "app.example.test", "app.example.test"},
		{"localhost", func(port string) string { return "localhost:" + port }, "app.example.test", "app.example.test"},
		{"no public name", func(port string) string { return "127.0.0.1:" + port }, "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			seen := make(chan string, 1)
			go func() {
				connection, err := listener.Accept()
				if err != nil {
					return
				}
				defer connection.Close()
				_ = tls.Server(connection, &tls.Config{GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
					seen <- hello.ServerName
					return nil, context.Canceled // the handshake fails; only the name the client sent matters
				}}).Handshake()
			}()
			_, port, _ := net.SplitHostPort(listener.Addr().String())
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = dialPreviewOrigin(ctx, LeaseTarget{Scheme: "https", Address: test.address(port), PublicHost: test.public})
			select {
			case got := <-seen:
				if got != test.want {
					t.Fatalf("SNI = %q, want %q", got, test.want)
				}
			case <-ctx.Done():
				t.Fatal("the origin saw no handshake")
			}
		})
	}
}
