// paperboat-relay runs private DERP/QUIC and DERP/WSS roles. It has no public HTTP ingress.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	"github.com/pinksaucepasta/paperboat-relay/internal/reporting"
	"github.com/pinksaucepasta/paperboat-relay/nodelifecycle"
	"github.com/pinksaucepasta/paperboat-relay/operator"
	"github.com/pinksaucepasta/paperboat-relay/peerrelay"
	"go4.org/mem"
	"tailscale.com/net/stunserver"
	"tailscale.com/types/key"
)

var version = "development"

func run(reporters ...*reporting.Reporter) error {
	var reporter *reporting.Reporter
	if len(reporters) > 0 {
		reporter = reporters[0]
	}
	if len(os.Args) == 2 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Fprintf(os.Stdout, "paperboat-relay %s\n", version)
		return nil
	}
	if handled, err := operator.Dispatch(context.Background(), os.Args[1:], "relay", os.Stdout, nil); handled {
		return err
	}
	if generated, handled, err := operator.RuntimeArgs(os.Args[1:]); handled {
		if err != nil {
			return err
		}
		os.Args = append([]string{os.Args[0]}, generated...)
	}
	listen := flag.String("listen", "", "UDP listen address")
	wssListen := flag.String("wss-listen", "", "WSS/TCP listen address")
	cert := flag.String("tls-cert", "", "TLS certificate file")
	private := flag.String("tls-key", "", "TLS private key file")
	keys := flag.String("jwks", "", "trusted Paperboat issuer JWKS file")
	issuer := flag.String("issuer", "", "Paperboat issuer")
	node := flag.String("node-id", "", "registered relay node ID")
	generation := flag.Uint64("node-generation", 0, "registered node generation")
	controlURL := flag.String("control-url", "", "authenticated control-plane HTTPS base URL")
	controlCA := flag.String("control-ca", "", "optional control-plane CA bundle")
	controlCredential := flag.String("control-credential-file", "", "control-plane credential file")
	nodeState := flag.String("node-state", "", "durable node startup state file")
	servicePath := flag.String("peer-relay-service", "", "registered public service descriptor JSON file")
	serviceKeyPath := flag.String("peer-relay-disco-key", "", "private service discovery key file (rawurl base64)")
	serviceAddresses := flag.String("peer-relay-addresses", "", "comma-separated advertised UDP relay IP:port addresses")
	flag.Parse()
	if *listen == "" || *wssListen == "" || *keys == "" {
		return errors.New("DERP/QUIC and WSS listen addresses and trusted JWKS are required")
	}
	_, quicPort, quicAddressErr := net.SplitHostPort(*listen)
	_, wssPort, wssAddressErr := net.SplitHostPort(*wssListen)
	quicPortNumber, quicPortErr := strconv.Atoi(quicPort)
	wssPortNumber, wssPortErr := strconv.Atoi(wssPort)
	if quicAddressErr != nil || wssAddressErr != nil || quicPortErr != nil || wssPortErr != nil || quicPortNumber < 1 || quicPortNumber > 65535 || wssPortNumber < 1 || wssPortNumber > 65535 || quicPortNumber == wssPortNumber {
		return errors.New("DERP/QUIC and WSS/STUN listen ports must be valid and distinct")
	}
	pair, err := tls.LoadX509KeyPair(*cert, *private)
	if err != nil {
		return errors.New("cannot load relay TLS certificate and key")
	}
	raw, err := os.ReadFile(*keys)
	if err != nil || len(raw) > 128<<10 {
		return errors.New("cannot read bounded issuer JWKS")
	}
	var jwks struct {
		Keys []struct{ Kty, Crv, Kid, X string }
	}
	if json.Unmarshal(raw, &jwks) != nil || len(jwks.Keys) == 0 || len(jwks.Keys) > 16 {
		return errors.New("invalid issuer JWKS")
	}
	trusted := map[string]ed25519.PublicKey{}
	for _, k := range jwks.Keys {
		b, e := base64.RawURLEncoding.Strict().DecodeString(k.X)
		if e != nil || k.Kty != "OKP" || k.Crv != "Ed25519" || k.Kid == "" || len(b) != 32 || trusted[k.Kid] != nil {
			return errors.New("invalid issuer signing key")
		}
		trusted[k.Kid] = ed25519.PublicKey(b)
	}
	credentialInfo, err := os.Stat(*controlCredential)
	if err != nil || credentialInfo.Size() > 4096 || credentialInfo.Mode().Perm()&0077 != 0 {
		return errors.New("cannot read private control credential file")
	}
	credential, err := os.ReadFile(*controlCredential)
	if err != nil {
		return errors.New("cannot read private control credential file")
	}
	var controlHTTP *http.Client
	if *controlCA != "" {
		raw, e := os.ReadFile(*controlCA)
		if e != nil {
			return errors.New("cannot read control CA")
		}
		roots, e := x509.SystemCertPool()
		if e != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(raw) {
			return errors.New("control CA contains no certificates")
		}
		controlHTTP = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}}
	}
	var controlTrace func(context.Context, string) (string, string, func(string, string))
	if reporter != nil {
		controlTrace = reporter.ControlTrace
	}
	control, err := nodelifecycle.New(nodelifecycle.Config{URL: *controlURL, Credential: strings.TrimSpace(string(credential)), NodeID: *node, ExpectedGeneration: *generation, StatePath: *nodeState, HTTP: controlHTTP, ControlTrace: controlTrace})
	if err != nil {
		return err
	}
	defer control.Close()
	startup, stopStartup := context.WithTimeout(context.Background(), 5*time.Second)
	lease, err := control.Start(startup)
	stopStartup()
	if err != nil {
		return err
	}
	server, err := derpquic.NewServer(derpquic.Verifier{Issuer: *issuer, NodeID: *node, NodeGeneration: lease.Generation, ProcessEpoch: lease.ProcessEpoch, Keys: trusted})
	if err != nil {
		return err
	}
	if err = server.SetConnectionLimit(int(lease.CapacityLimit)); err != nil {
		return err
	}
	var peerService *peerrelay.Server
	if *servicePath != "" || *serviceKeyPath != "" || *serviceAddresses != "" {
		if *servicePath == "" || *serviceKeyPath == "" || *serviceAddresses == "" {
			return errors.New("peer relay requires its registered descriptor, discovery key and UDP addresses")
		}
		raw, e := os.ReadFile(*servicePath)
		if e != nil || len(raw) > 4096 {
			return errors.New("cannot read peer relay descriptor")
		}
		var descriptor derpquic.ServiceDescriptor
		if json.Unmarshal(raw, &descriptor) != nil || !descriptor.Valid() {
			return errors.New("invalid peer relay descriptor")
		}
		encoded, e := os.ReadFile(*serviceKeyPath)
		if e != nil || len(encoded) > 128 {
			return errors.New("cannot read peer relay discovery key")
		}
		private, e := base64.RawURLEncoding.Strict().DecodeString(strings.TrimSpace(string(encoded)))
		if e != nil || len(private) != 32 || private[0]&7 != 0 || private[31]&128 != 0 || private[31]&64 == 0 {
			return errors.New("invalid peer relay discovery key")
		}
		var addresses []netip.AddrPort
		for _, value := range strings.Split(*serviceAddresses, ",") {
			a, e := netip.ParseAddrPort(value)
			if e != nil {
				return errors.New("invalid peer relay UDP address")
			}
			addresses = append(addresses, a)
		}
		relay, e := peerrelay.New(peerrelay.Config{Service: descriptor, DiscoPrivate: key.DiscoPrivateFromRaw32(mem.B(private)), Port: addresses[0].Port(), Addresses: addresses})
		if e != nil {
			return e
		}
		defer relay.Close()
		peerService = relay
		if e = server.SetControlService(descriptor, relay.Handle); e != nil {
			return e
		}
	}
	if !server.MatchesControlService(lease.PeerRelay) {
		return errors.New("configured peer relay service does not match registered node")
	}
	socket, err := net.ListenPacket("udp", *listen)
	if err != nil {
		return errors.New("cannot bind relay UDP listener")
	}
	defer socket.Close()
	tcp, err := net.Listen("tcp", *wssListen)
	if err != nil {
		return errors.New("cannot bind relay WSS listener")
	}
	defer tcp.Close()
	wssTLS := &tls.Config{Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequestClientCert, MinVersion: tls.VersionTLS12}
	httpServer := &http.Server{Handler: server.WSSHandler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 45 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The operator-approved WSS TCP port also serves STUN over UDP. This
	// separate socket lets magicsock discover public endpoints without making
	// QUIC packet handling depend on unauthenticated STUN traffic.
	stun := stunserver.New(ctx)
	if err := stun.Listen(*wssListen); err != nil {
		return errors.New("cannot bind relay STUN listener")
	}
	var metricsDone chan struct{}
	if reporter != nil {
		metricsDone = make(chan struct{})
		go func() {
			defer close(metricsDone)
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			previous := server.Snapshot()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					current := server.Snapshot()
					reporter.ExportDrops(ctx)
					snapshots := []reporting.Snapshot{{Name: "paperboat_relay_sessions", Kind: "gauge", Value: float64(current.Connections)}, {Name: "paperboat_relay_capacity_limit", Kind: "gauge", Value: float64(lease.CapacityLimit)}, {Name: "paperboat_relay_admissions_total", Kind: "counter", Value: float64(current.Accepted), Labels: map[string]string{"outcome": "success"}}, {Name: "paperboat_relay_admissions_total", Kind: "counter", Value: float64(current.Denied), Labels: map[string]string{"outcome": "rejected"}}, {Name: "paperboat_relay_packets_total", Kind: "counter", Value: float64(current.Forwarded), Labels: map[string]string{"outcome": "forwarded"}}, {Name: "paperboat_relay_packets_total", Kind: "counter", Value: float64(current.Dropped), Labels: map[string]string{"outcome": "dropped"}}}
					if peerService != nil {
						peer := peerService.Snapshot()
						snapshots = append(snapshots, reporting.Snapshot{Name: "paperboat_peer_relay_allocations", Kind: "gauge", Value: float64(peer.Allocations)}, reporting.Snapshot{Name: "paperboat_peer_relay_admissions_total", Kind: "counter", Value: float64(peer.Admitted), Labels: map[string]string{"outcome": "success"}}, reporting.Snapshot{Name: "paperboat_peer_relay_admissions_total", Kind: "counter", Value: float64(peer.Denied), Labels: map[string]string{"outcome": "rejected"}}, reporting.Snapshot{Name: "paperboat_peer_relay_packets_total", Kind: "counter", Value: float64(peer.AuthorizedPackets)})
					}
					reporter.MetricSnapshots(ctx, snapshots)
					if current.Denied > previous.Denied {
						reporter.Observe(ctx, "relay_admission", "rejected", "unauthorized", "", 0)
					}
					if current.Accepted > previous.Accepted {
						reporter.Observe(ctx, "relay_session", "success", "ok", "", 0)
					}
					if current.Dropped > previous.Dropped {
						reporter.Observe(ctx, "relay_session", "failed", "capacity", "", 0)
					}
					previous = current
				}
			}
		}()
		defer func() { cancel(); <-metricsDone }()
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	stopped := make(chan struct{})
	defer func() { cancel(); <-stopped }()
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			return
		case <-signals:
			server.Drain(time.Now().Add(5 * time.Second))
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			select {
			case <-ctx.Done():
			case <-timer.C:
				cancel()
			}
		}
	}()
	errs := make(chan error, 4)
	go func() { errs <- server.Serve(ctx, socket, &tls.Config{Certificates: []tls.Certificate{pair}}) }()
	go func() { errs <- httpServer.Serve(tls.NewListener(tcp, wssTLS)) }()
	go func() { errs <- stun.Serve() }()
	controlDone := make(chan struct{})
	go func() { defer close(controlDone); errs <- control.Run(ctx, server) }()
	err = <-errs
	wasCanceled := ctx.Err() != nil
	cancel()
	<-controlDone
	server.Drain(time.Now())
	observe, stopObserve := context.WithTimeout(context.Background(), time.Second)
	_ = control.Observe(observe, server, true)
	stopObserve()
	server.Close()
	shutdown, stopShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	_ = httpServer.Shutdown(shutdown)
	stopShutdown()
	server.Wait()
	if wasCanceled || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return serviceFailure{err}
}

type serviceFailure struct{ error }

func main() {
	reporter, err := reporting.New("paperboat-relay")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := execute(reporter)
	reporter.Close()
	os.Exit(code)
}

func execute(reporter *reporting.Reporter) (code int) {
	started := time.Now()
	defer func() {
		if recover() != nil {
			reference := reportUnexpected(reporter, "panic")
			reporter.Observe(context.Background(), "service_lifecycle", "failed", "internal", reference, time.Since(started))
			code = 2
		}
	}()
	if err := run(reporter); err != nil {
		var failure serviceFailure
		if errors.As(err, &failure) {
			reference := reportUnexpected(reporter, "service_run")
			reporter.Observe(context.Background(), "service_lifecycle", "failed", "internal", reference, time.Since(started))
		} else {
			reporter.Observe(context.Background(), "service_lifecycle", "rejected", "invalid", "", time.Since(started))
			fmt.Fprintln(os.Stderr, err)
		}
		return 1
	}
	reporter.Observe(context.Background(), "service_lifecycle", "success", "shutdown", "", time.Since(started))
	return 0
}

func reportUnexpected(reporter *reporting.Reporter, kind string) string {
	reference := reporting.Reference()
	if reference == "" {
		fmt.Fprintln(os.Stderr, "paperboat-relay stopped unexpectedly")
		return ""
	}
	reporter.Capture(reference, kind, 2)
	fmt.Fprintf(os.Stderr, "paperboat-relay stopped unexpectedly; support reference %s\n", reference)
	return reference
}
