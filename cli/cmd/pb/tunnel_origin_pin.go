package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
	sharedprotocol "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

var tunnelTLSPinPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// fetchOriginPin reads the certificate the origin presents (as seen from this machine), shows it and
// asks to pin it, SSH known_hosts style. Without a terminal it fails with the --tls-pin to pass.
func fetchOriginPin(command *cobra.Command, address string, serverName *string) (string, error) {
	config := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true} //nolint:gosec // reading the certificate to show and pin it
	if serverName != nil {
		config.ServerName = *serverName
	} else if host := sharedprotocol.OriginHost(address); net.ParseIP(host) == nil {
		config.ServerName = host
	}
	parent := command.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	connection, err := (&tls.Dialer{Config: config}).DialContext(ctx, "tcp", address)
	if err != nil {
		return "", fmt.Errorf("read the origin certificate from %s: %w (pass --tls-pin sha256:<fingerprint> to pin without connecting)", address, err)
	}
	defer connection.Close()
	certificates := connection.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certificates) == 0 {
		return "", errors.New("the origin presented no certificate")
	}
	leaf := certificates[0]
	pin := tunnelmanager.CertificateFingerprint(leaf)
	out := command.ErrOrStderr()
	fmt.Fprintf(out, "Origin certificate at %s (as seen from this machine):\n  subject  %s\n  issuer   %s\n  expires  %s\n  sha256   %s\n",
		address, leaf.Subject, leaf.Issuer, leaf.NotAfter.Format(time.DateOnly), strings.TrimPrefix(pin, "sha256:"))
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", fmt.Errorf("confirm the certificate by passing --tls-pin %s", pin)
	}
	fmt.Fprint(out, "Pin this certificate for the route? [y/N] ")
	answer, _ := bufio.NewReader(command.InOrStdin()).ReadString('\n')
	if answer = strings.ToLower(strings.TrimSpace(answer)); answer != "y" && answer != "yes" {
		return "", errors.New("not pinned; the route was not changed")
	}
	return pin, nil
}

// originCertificateHint names what the certificate an https origin presents means behind Paperboat
// and the route setting that fixes it; "" when it verifies for serverName.
func originCertificateHint(leaf *x509.Certificate, serverName string, now time.Time) string {
	_, chainErr := leaf.Verify(x509.VerifyOptions{DNSName: serverName, CurrentTime: now})
	switch {
	case chainErr == nil:
		return ""
	case strings.Contains(leaf.Issuer.CommonName, "Caddy Local Authority"):
		return "it is Caddy's internal certificate (tls internal), renewed every few hours: use --tls-verification insecure for a local origin, --ca-reference with Caddy's root, or an http:// origin"
	case leaf.Subject.CommonName == "TRAEFIK DEFAULT CERT":
		return "Traefik answered with its default certificate, so it has no certificate for this name (its ACME cannot work behind Paperboat): point the route at Traefik's http entrypoint, or use --tls-verification insecure for a local origin"
	case now.After(leaf.NotAfter):
		return "it expired on " + leaf.NotAfter.Format(time.DateOnly) + ": renew it on the origin, or pin the new one with --tls-verification pinned"
	case leaf.VerifyHostname(serverName) != nil && len(leaf.DNSNames) > 0:
		return "it is for " + strings.Join(leaf.DNSNames, ", ") + ", not " + serverName + ": set --tls-server-name " + leaf.DNSNames[0]
	case leaf.Issuer.String() == leaf.Subject.String():
		return "it is self-signed: pin it with --tls-verification pinned (or --tls-verification insecure for a local origin)"
	default:
		return "it is signed by " + leaf.Issuer.String() + ", which this machine does not trust: give that CA with --ca-reference, pin it with --tls-verification pinned, or use an http:// origin"
	}
}

// writeTunnelOriginCertificateHints explains, for each https route, a certificate the origin presents
// that does not verify, as seen from this machine (run doctor on the machine that serves the tunnel).
func writeTunnelOriginCertificateHints(ctx context.Context, writer io.Writer, client *api.Client, tunnelID string) {
	page, err := client.ListTunnelRoutesV1(ctx, tunnelID, "", 200)
	if err != nil {
		return
	}
	publicHost := ""
	if tunnel, err := client.GetTunnelV1(ctx, tunnelID); err == nil {
		publicHost = strings.TrimPrefix(tunnel.StableEndpoint, "https://")
	}
	for _, route := range page.Items {
		if route.Origin.Scheme != "https" || route.Origin.TLS == nil || route.Origin.TLS.Verification == "insecure" || route.Origin.TLS.Verification == "pinned" {
			continue
		}
		serverName := sharedprotocol.OriginHost(route.Origin.Address)
		if route.Origin.TLS.ServerName != nil {
			serverName = *route.Origin.TLS.ServerName
		} else if route.HostMatch.Hostname != "" && !strings.HasPrefix(route.HostMatch.Hostname, "*.") && sharedprotocol.OriginHostIsLocalhostOrIP(route.Origin.Address) {
			serverName = route.HostMatch.Hostname
		} else if publicHost != "" && sharedprotocol.OriginHostIsLocalhostOrIP(route.Origin.Address) {
			serverName = publicHost
		}
		dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		connection, err := (&tls.Dialer{Config: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName, InsecureSkipVerify: true}}).DialContext(dialCtx, "tcp", route.Origin.Address) //nolint:gosec // reading the certificate to explain it
		cancel()
		if err != nil {
			continue
		}
		certificates := connection.(*tls.Conn).ConnectionState().PeerCertificates
		_ = connection.Close()
		if len(certificates) == 0 {
			continue
		}
		if hint := originCertificateHint(certificates[0], serverName, time.Now()); hint != "" {
			_, _ = fmt.Fprintf(writer, "Origin certificate (route %s, seen from this machine): %s.\n", route.Name, hint)
		}
	}
}
