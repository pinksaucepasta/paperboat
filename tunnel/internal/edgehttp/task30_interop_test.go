package edgehttp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/route"
)

type task30CarrierNode struct{ Address, NodeID, Epoch, Domain string }
type task30EdgeFixture struct {
	AdditionalCarrierPath                                                       string
	LifetimeSeconds                                                             int
	DockerSimulation                                                            bool
	PublicTLSListenHost                                                         string `json:"public_tls_listen_host,omitempty"`
	DynamicAuthority                                                            bool   `json:"dynamic_authority,omitempty"`
	Node                                                                        task30CarrierNode
	IP, HTTPPort, TCPPort, CA, Cert, Key, CarrierPath, AuthorityPath, ReadyPath string
	CarrierListenAddress                                                        string `json:"carrier_listen_address,omitempty"`
	PublicHTTPListenAddress                                                     string `json:"public_http_listen_address,omitempty"`
	PublicTCPListenAddress                                                      string `json:"public_tcp_listen_address,omitempty"`
}

const task30AuthorityMaximumBytes = 1 << 20

type task35cCAPublicFixture struct {
	Schema string `json:"schema"`
	CACert string `json:"ca_cert"`
}

type task35cManifest struct {
	Schema        string   `json:"schema"`
	CAPath        string   `json:"ca_path"`
	DaemonPath    string   `json:"daemon_path"`
	EdgePaths     []string `json:"edge_paths"`
	RelayHosts    []string `json:"relay_hosts"`
	CarrierListen string   `json:"carrier_listen_address"`
	HTTPListen    string   `json:"public_http_listen_address"`
	TCPListen     string   `json:"public_tcp_listen_address"`
}

type task35cEdgeCertificate struct {
	Cert string
	Key  string
}

func task30Write(t *testing.T, path string, value any) {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}
func task30Read(t *testing.T, ctx context.Context, path string, value any) {
	t.Helper()
	for {
		b, err := os.ReadFile(path)
		if err == nil && json.Unmarshal(b, value) == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// task30ReadCurrentAuthority is deliberately a one-shot read. The remote
// Task 35c edge calls it for every HTTP and TCP ingress, so a missing, malformed
// or expired file denies that ingress instead of reusing a prior decision or
// manufacturing a new validity window locally.
func task30ReadCurrentAuthority(ctx context.Context, path, protocol string) (connectorprotocol.IngressDecision, error) {
	if ctx == nil || strings.TrimSpace(path) == "" || (protocol != "http" && protocol != "tcp") {
		return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
	}
	select {
	case <-ctx.Done():
		return connectorprotocol.IngressDecision{}, ctx.Err()
	default:
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 || len(b) > task30AuthorityMaximumBytes {
		return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
	}
	var authority task24Authority
	if err := json.Unmarshal(b, &authority); err != nil {
		return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
	}
	decision := authority.HTTP
	if protocol == "tcp" {
		decision = authority.TCP
	}
	if decision.Validate(time.Now().UTC()) != nil {
		return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
	}
	return decision, nil
}

// task30WaitCurrentAuthority is only used during dynamic-edge startup. It
// waits for the coordinator's first atomic fixture publication, but once a
// fixture exists an invalid or expired decision is a hard startup failure.
func task30WaitCurrentAuthority(ctx context.Context, path, protocol string) (connectorprotocol.IngressDecision, error) {
	for {
		decision, err := task30ReadCurrentAuthority(ctx, path, protocol)
		if err == nil {
			return decision, nil
		}
		if _, statErr := os.Stat(path); statErr == nil {
			return connectorprotocol.IngressDecision{}, err
		}
		select {
		case <-ctx.Done():
			return connectorprotocol.IngressDecision{}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func task30ListenAddress(explicit, fallback string) string {
	if address := strings.TrimSpace(explicit); address != "" {
		return address
	}
	return fallback
}

func task35cRequiredEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("%s is required", name)
	}
	return value
}

func task35cHosts(t *testing.T, raw string) []string {
	t.Helper()
	raw = strings.ReplaceAll(raw, ",", " ")
	parts := strings.Fields(raw)
	if len(parts) != 2 {
		t.Fatalf("PAPERBOAT_TASK35C_PUBLIC_RELAY_HOSTS must contain exactly two hosts")
	}
	hosts := make([]string, 2)
	for i, value := range parts {
		value = strings.Trim(value, "[]")
		if value == "" || len(value) > 253 || strings.ContainsAny(value, "/\\\r\n\t") {
			t.Fatalf("invalid public relay host %q", value)
		}
		if net.ParseIP(value) == nil && strings.Contains(value, ":") {
			t.Fatalf("invalid public relay host %q", value)
		}
		hosts[i] = value
	}
	return hosts
}

func task35cPort(t *testing.T, envName, fallback string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(envName))
	if value == "" {
		value = fallback
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		t.Fatalf("%s must be a TCP port in 1..65535", envName)
	}
	return strconv.Itoa(port)
}

func task35cListenPort(t *testing.T, name, address string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("invalid %s listen address %q: %v", name, address, err)
	}
	if port == "" {
		t.Fatalf("invalid %s listen address %q: missing port", name, address)
	}
	return task35cPortValue(t, name, port)
}

func task35cPortValue(t *testing.T, name, value string) string {
	t.Helper()
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		t.Fatalf("invalid %s listen port %q", name, value)
	}
	return strconv.Itoa(port)
}

func task35cLifetime(t *testing.T) int {
	t.Helper()
	value := strings.TrimSpace(os.Getenv("PAPERBOAT_TASK35C_LIFETIME_SECONDS"))
	if value == "" {
		return 840
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 5 || seconds > 900 {
		t.Fatalf("PAPERBOAT_TASK35C_LIFETIME_SECONDS must be 5..900")
	}
	return seconds
}

func task35cWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) == 0 || len(encoded) > task30AuthorityMaximumBytes {
		t.Fatalf("fixture %s exceeds %d-byte bound", path, task30AuthorityMaximumBytes)
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	temporary, err := os.CreateTemp(directory, ".task35c-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		t.Fatal(err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		t.Fatal(err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		t.Fatal(err)
	}
	if err := temporary.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		if err == nil {
			err = fmt.Errorf("permissions %o", info.Mode().Perm())
		}
		t.Fatalf("fixture %s is not mode 0600: %v", path, err)
	}
}

func task35cCertificates(t *testing.T, hosts []string, publicHostname string) ([]byte, []task35cEdgeCertificate, []byte, []byte) {
	t.Helper()
	_, caKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "paperboat task35c fixture CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	issue := func(serial int64, commonName string, server bool, host string) ([]byte, []byte) {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		dnsNames := []string{"localhost", "app.customer.test", publicHostname}
		var ipAddresses []net.IP
		if ip := net.ParseIP(host); ip != nil {
			ipAddresses = []net.IP{ip}
		} else if host != "" {
			dnsNames = append(dnsNames, host)
		}
		usage := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		if server {
			usage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: commonName}, DNSNames: dnsNames, IPAddresses: ipAddresses, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usage}
		der, err := x509.CreateCertificate(rand.Reader, template, caCert, key.Public(), caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	}
	edges := make([]task35cEdgeCertificate, len(hosts))
	for i, host := range hosts {
		cert, key := issue(int64(i+2), "task35c-edge-"+strconv.Itoa(i+1), true, host)
		edges[i] = task35cEdgeCertificate{Cert: string(cert), Key: string(key)}
	}
	clientCert, clientKey := issue(10, "task35c-daemon", false, "")
	return caPEM, edges, clientCert, clientKey
}

// TestTask35CPrepare emits only bounded, task-owned fixture files. It is an
// opt-in coordinator for two public edge hosts; it does not contact either
// host, start a service, or create production authority. The CA is a fixture
// CA and its signing key is retained only in memory, never serialized.
func TestTask35CPrepare(t *testing.T) {
	if strings.TrimSpace(os.Getenv("PAPERBOAT_TASK35C_ROLE")) != "prepare" {
		t.Skip("set PAPERBOAT_TASK35C_ROLE=prepare to emit Task 35c fixtures")
	}
	directory, err := filepath.Abs(task35cRequiredEnv(t, "PAPERBOAT_TASK35C_FIXTURE_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	hosts := task35cHosts(t, task35cRequiredEnv(t, "PAPERBOAT_TASK35C_PUBLIC_RELAY_HOSTS"))
	publicHostname := task35cRequiredEnv(t, "PAPERBOAT_TASK35C_PUBLIC_HOSTNAME")
	carrierPort := task35cPort(t, "PAPERBOAT_TASK35C_CARRIER_PORT", "27443")
	httpPort := task35cPort(t, "PAPERBOAT_TASK35C_HTTP_PORT", "28443")
	tcpPort := task35cPort(t, "PAPERBOAT_TASK35C_TCP_PORT", "25001")
	carrierListen := task30ListenAddress(os.Getenv("PAPERBOAT_TASK35C_CARRIER_LISTEN_ADDRESS"), net.JoinHostPort("0.0.0.0", carrierPort))
	httpListen := task30ListenAddress(os.Getenv("PAPERBOAT_TASK35C_PUBLIC_HTTP_LISTEN_ADDRESS"), net.JoinHostPort("0.0.0.0", httpPort))
	tcpListen := task30ListenAddress(os.Getenv("PAPERBOAT_TASK35C_PUBLIC_TCP_LISTEN_ADDRESS"), net.JoinHostPort("0.0.0.0", tcpPort))
	carrierPort = task35cListenPort(t, "carrier", carrierListen)
	httpPort = task35cListenPort(t, "public HTTP", httpListen)
	tcpPort = task35cListenPort(t, "public TCP", tcpListen)
	lifetime := task35cLifetime(t)
	ca, edgeCertificates, clientCert, clientKey := task35cCertificates(t, hosts, publicHostname)
	caPath := filepath.Join(directory, "ca.json")
	daemonPath := filepath.Join(directory, "daemon.json")
	edgePaths := make([]string, len(hosts))
	nodes := make([]task30CarrierNode, len(hosts))
	task35cWriteJSON(t, caPath, task35cCAPublicFixture{Schema: "paperboat.task35c.fixture-ca.v1", CACert: string(ca)})
	for i, host := range hosts {
		prefix := filepath.Join(directory, fmt.Sprintf("edge-%d", i+1))
		node := task30CarrierNode{Address: net.JoinHostPort(host, carrierPort), NodeID: fmt.Sprintf("task35c_edge_%02d", i+1), Epoch: fmt.Sprintf("task35c_epoch_%02d", i+1), Domain: host}
		nodes[i] = node
		fixture := task30EdgeFixture{LifetimeSeconds: lifetime, DynamicAuthority: true, Node: node, IP: host, HTTPPort: httpPort, TCPPort: tcpPort, CA: string(ca), Cert: edgeCertificates[i].Cert, Key: edgeCertificates[i].Key, CarrierPath: prefix + "-carrier.json", AuthorityPath: prefix + "-authority.json", ReadyPath: prefix + "-ready.json", CarrierListenAddress: carrierListen, PublicHTTPListenAddress: httpListen, PublicTCPListenAddress: tcpListen}
		edgePaths[i] = prefix + ".json"
		task35cWriteJSON(t, edgePaths[i], fixture)
	}
	descriptor := task24Descriptor{Protocol: "http2", CACert: string(ca), ClientCert: string(clientCert), ClientKey: string(clientKey), OriginsPath: filepath.Join(directory, "origins.json"), AuthorityPath: filepath.Join(directory, "authority.json"), ReadyPath: filepath.Join(directory, "daemon-ready.json"), StopPath: filepath.Join(directory, "stop"), Nodes: nodes}
	task35cWriteJSON(t, daemonPath, descriptor)
	task35cWriteJSON(t, filepath.Join(directory, "manifest.json"), task35cManifest{Schema: "paperboat.task35c.edge-fixtures.v1", CAPath: caPath, DaemonPath: daemonPath, EdgePaths: edgePaths, RelayHosts: hosts, CarrierListen: carrierListen, HTTPListen: httpListen, TCPListen: tcpListen})
	t.Logf("Task 35c fixture CA (explicit test authority) and two edge/daemon descriptors written under %s", directory)
}

// Each helper is a separate OS process with its own carrier, routing and public listeners.
func TestTask30EdgeProcess(t *testing.T) {
	path := os.Getenv("PAPERBOAT_TASK30_EDGE")
	dynamicProcess := false
	if path == "" {
		path = os.Getenv("PAPERBOAT_TASK35C_EDGE")
		dynamicProcess = path != ""
	}
	if path == "" {
		t.Skip("coordinator helper; set PAPERBOAT_TASK30_EDGE or PAPERBOAT_TASK35C_EDGE")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Second)
	defer cancel()
	var f task30EdgeFixture
	task30Read(t, ctx, path, &f)
	if dynamicProcess {
		f.DynamicAuthority = true
	}
	var initialDynamicDecision connectorprotocol.IngressDecision
	if f.DynamicAuthority {
		decision, waitErr := task30WaitCurrentAuthority(ctx, f.AuthorityPath, "http")
		if waitErr != nil {
			t.Fatal(waitErr)
		}
		initialDynamicDecision = decision
	}
	lifetime := 30 * time.Second
	if f.DockerSimulation {
		lifetime = 120 * time.Second
		if f.LifetimeSeconds >= 120 && f.LifetimeSeconds <= 900 {
			lifetime = time.Duration(f.LifetimeSeconds) * time.Second
		}
	} else if f.DynamicAuthority {
		lifetime = 900 * time.Second
		if f.LifetimeSeconds >= 5 && f.LifetimeSeconds <= 900 {
			lifetime = time.Duration(f.LifetimeSeconds) * time.Second
		}
	}
	timer := time.AfterFunc(lifetime, cancel)
	defer timer.Stop()
	certificate, err := tls.X509KeyPair([]byte(f.Cert), []byte(f.Key))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(f.CA)) {
		t.Fatal("invalid CA")
	}
	identity := replicaIdentity("host_pinned", "connector_pinned", "session_pinned", 3)
	if f.DynamicAuthority {
		identity.AccountID = initialDynamicDecision.Binding.AccountID
		identity.HostID = initialDynamicDecision.Binding.HostID
		identity.TunnelID = initialDynamicDecision.Binding.TunnelID
		identity.ConnectorID = initialDynamicDecision.ConnectorID
		identity.SessionID = initialDynamicDecision.SessionID
		identity.ProcessGeneration = initialDynamicDecision.ProcessGeneration
		identity.Generation = initialDynamicDecision.ConfigGeneration
	}
	listenAddress := task30ListenAddress(f.CarrierListenAddress, "127.0.0.1:0")
	if f.DockerSimulation && strings.TrimSpace(f.CarrierListenAddress) == "" {
		listenAddress = "0.0.0.0:27443"
	}
	endpoint := datacarrier.EndpointConfig{Address: listenAddress, TLS: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}, PeerBinding: func(tls.ConnectionState) (datacarrier.Identity, error) { return identity, nil }}
	config := datacarrier.ServiceConfig{TCP: &endpoint, Carrier: datacarrier.DefaultConfig()}
	config.Carrier.Authorize = datacarrier.AuthorizerFunc(func(_ context.Context, _ datacarrier.Identity, o datacarrier.StreamOpen) error { return o.Validate() })
	service, err := datacarrier.NewHTTPService(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if !f.DockerSimulation && f.Node.Address == "" {
		f.Node.Address = service.TCPAddr().String()
		if host := strings.TrimSpace(f.IP); host != "" && host != "0.0.0.0" && host != "::" {
			if _, port, splitErr := net.SplitHostPort(f.Node.Address); splitErr == nil {
				f.Node.Address = net.JoinHostPort(host, port)
			}
		}
	}
	var extraService *datacarrier.HTTPService
	var extraDecision connectorprotocol.IngressDecision
	if f.AdditionalCarrierPath != "" {
		decisions, err := (task365FileAuthority{path: f.AuthorityPath}).Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, decision := range decisions {
			if decision.Binding.TunnelID != identity.TunnelID {
				extraDecision = decision
				break
			}
		}
		if extraDecision.Binding.TunnelID == "" {
			t.Fatal("second tunnel authority missing")
		}
		extraIdentity := datacarrier.Identity{AccountID: extraDecision.Binding.AccountID, HostID: extraDecision.Binding.HostID, TunnelID: extraDecision.Binding.TunnelID, ConnectorID: extraDecision.ConnectorID, SessionID: extraDecision.SessionID, ProcessGeneration: extraDecision.ProcessGeneration, Generation: extraDecision.ConfigGeneration}
		extraEndpoint := endpoint
		extraEndpoint.PeerBinding = func(tls.ConnectionState) (datacarrier.Identity, error) { return extraIdentity, nil }
		extraConfig := config
		extraConfig.TCP = &extraEndpoint
		extraService, err = datacarrier.NewHTTPService(ctx, extraConfig)
		if err != nil {
			t.Fatal(err)
		}
		defer extraService.Close()
		extraNode := f.Node
		extraNode.Address = extraService.TCPAddr().String()
		task30Write(t, f.AdditionalCarrierPath, extraNode)
	}
	task30Write(t, f.CarrierPath, f.Node)
	var authority task24Authority
	if f.DynamicAuthority {
		authority.HTTP = initialDynamicDecision
	} else {
		task30Read(t, ctx, f.AuthorityPath, &authority)
	}
	server, err := service.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	var gateway *Policy
	var registry *DataCarrierRouteRegistry
	if f.DynamicAuthority {
		gateway, registry = task30RouteGatewayWithAuthority(t, ctx, server, authority.HTTP, func(currentCtx context.Context, current route.RouteRule) (connectorprotocol.IngressDecision, error) {
			if current.RouteID != authority.HTTP.Binding.RouteID {
				return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
			}
			return task30ReadCurrentAuthority(currentCtx, f.AuthorityPath, "http")
		})
	} else {
		gateway, registry = task24RouteGateway(t, ctx, server, authority.HTTP, f.DockerSimulation)
	}
	if extraService != nil {
		extraServer, err := extraService.Accept(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer extraServer.Close()
		routeID := extraDecision.Binding.RouteID
		state := ReplicaState{Ready: true, Generation: extraDecision.ConfigGeneration, RouteIDs: []string{routeID}, HealthyRoutes: []string{routeID}, FailureDomain: f.Node.Domain, Latency: time.Millisecond, Capacity: 4, RouteBindings: []ReplicaRouteBinding{{RouteID: routeID, AssignmentID: "assignment_task365_second", AssignmentGeneration: extraDecision.AssignmentGeneration, RouteGeneration: extraDecision.Binding.RouteGeneration, ConfigContentHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}}
		if err := registry.AttachReplica(extraServer, "task365-second-key", "task365-second-thumb", state); err != nil {
			t.Fatal(err)
		}
	}
	webAddress := task30ListenAddress(f.PublicHTTPListenAddress, net.JoinHostPort(f.IP, f.HTTPPort))
	webListener, err := net.Listen("tcp", webAddress)
	if err != nil {
		t.Fatal(err)
	}
	handler := http.Handler(gateway)
	if f.DockerSimulation {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Task30-Edge", f.Node.NodeID)
			gateway.ServeHTTP(w, r)
		})
	}
	web := &http.Server{Handler: handler, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}}}
	defer web.Close()
	if f.PublicTLSListenHost != "" {
		shared, err := NewSharedTLSListener(webListener, task365FileAuthority{path: f.AuthorityPath}, registry, func(host string) bool {
			current, err := task30ReadCurrentAuthority(ctx, f.AuthorityPath, "http")
			return err == nil && current.Binding.Hostname == host
		}, 128, "")
		if err != nil {
			_ = webListener.Close()
			t.Fatal(err)
		}
		webListener = shared
	}
	go web.ServeTLS(webListener, "", "")
	tcpAddress := task30ListenAddress(f.PublicTCPListenAddress, net.JoinHostPort(f.IP, f.TCPPort))
	tcpListener, err := net.Listen("tcp", tcpAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer tcpListener.Close()
	go func() {
		for {
			conn, err := tcpListener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if f.DynamicAuthority {
					initial, err := task30ReadCurrentAuthority(ctx, f.AuthorityPath, "tcp")
					if err != nil {
						return
					}
					_ = registry.ForwardPublicTCP(ctx, conn, initial, func(currentCtx context.Context, _ connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
						return task30ReadCurrentAuthority(currentCtx, f.AuthorityPath, "tcp")
					})
					return
				}
				initial := authority.TCP
				if f.DockerSimulation {
					initial.IssuedAt = time.Now().UTC()
					initial.ExpiresAt = initial.IssuedAt.Add(10 * time.Second)
				}
				_ = registry.ForwardPublicTCP(ctx, conn, initial, func(context.Context, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
					d := authority.TCP
					if f.DockerSimulation {
						d.IssuedAt = time.Now().UTC()
						d.ExpiresAt = d.IssuedAt.Add(10 * time.Second)
					}
					return d, nil
				})
			}()
		}
	}()
	task30Write(t, f.ReadyPath, true)
	<-ctx.Done()
}

// This fixture selects concrete cached addresses; it does not claim public DNS recovery.
func TestTask30TwoEdgeProcessRecovery(t *testing.T) {
	daemonBinary := os.Getenv("PAPERBOAT_TASK30_DAEMON_TEST_BINARY")
	if daemonBinary == "" {
		t.Skip("requires explicitly built daemon test binary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	directory := t.TempDir()
	_, serverTLS, ca, clientCert, clientKey := task24Certificates(t)
	cert := serverTLS.Certificates[0]
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	freePort := func() string {
		l, err := net.Listen("tcp", "127.0.0.2:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	}
	httpPort, tcpPort := freePort(), freePort()
	var children []*exec.Cmd
	outputs := map[*exec.Cmd]*bytes.Buffer{}
	start := func(binary, run, env string) *exec.Cmd {
		c := exec.CommandContext(ctx, binary, "-test.run=^"+run+"$", "-test.count=1")
		c.Env = append(os.Environ(), env)
		output := &bytes.Buffer{}
		c.Stdout = output
		c.Stderr = output
		outputs[c] = output
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, c)
		return c
	}
	defer func() {
		for _, c := range children {
			_ = c.Process.Kill()
			_ = c.Wait()
			if t.Failed() {
				t.Logf("child output: %s", outputs[c].String())
			}
		}
	}()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	nodes := make([]task30CarrierNode, 2)
	fixtures := make([]task30EdgeFixture, 2)
	for i := range 2 {
		prefix := filepath.Join(directory, fmt.Sprint(i))
		f := task30EdgeFixture{Node: task30CarrierNode{NodeID: fmt.Sprintf("edge_0%d", i+1), Epoch: fmt.Sprintf("epoch_000%d", i+1), Domain: fmt.Sprintf("domain_%d", i)}, IP: fmt.Sprintf("127.0.0.%d", i+2), HTTPPort: httpPort, TCPPort: tcpPort, CA: string(ca), Cert: string(certPEM), Key: string(keyPEM), CarrierPath: prefix + "-carrier", AuthorityPath: prefix + "-authority", ReadyPath: prefix + "-ready"}
		task30Write(t, prefix+"-fixture", f)
		start(self, "TestTask30EdgeProcess", "PAPERBOAT_TASK30_EDGE="+prefix+"-fixture")
		task30Read(t, ctx, f.CarrierPath, &nodes[i])
		fixtures[i] = f
	}
	descriptor := task24Descriptor{Protocol: "http2", CACert: string(ca), ClientCert: string(clientCert), ClientKey: string(clientKey), OriginsPath: filepath.Join(directory, "origins"), AuthorityPath: filepath.Join(directory, "daemon-authority"), ReadyPath: filepath.Join(directory, "daemon-ready"), StopPath: filepath.Join(directory, "stop"), Nodes: nodes}
	descriptorPath := filepath.Join(directory, "daemon-descriptor")
	task30Write(t, descriptorPath, descriptor)
	start(daemonBinary, "TestTask24CrossRepositoryDaemon", "PAPERBOAT_TASK24_DESCRIPTOR="+descriptorPath)
	var origins task24Origins
	task30Read(t, ctx, descriptor.OriginsPath, &origins)
	identity := replicaIdentity("host_pinned", "connector_pinned", "session_pinned", 3)
	now := time.Now().UTC()
	port, _ := strconv.Atoi(tcpPort)
	decision := func(node task30CarrierNode, protocol, origin string) connectorprotocol.IngressDecision {
		id := "route_" + protocol
		host := "app.customer.test"
		listener := ""
		publicPort := uint16(0)
		path := "/"
		if protocol == "tcp" {
			host = "tcp.customer.test"
			listener = "listener_task30"
			publicPort = uint16(port)
			path = ""
		}
		return connectorprotocol.IngressDecision{Binding: connectorprotocol.IngressBinding{EnvironmentID: "environment_task24", AccountID: identity.AccountID, TunnelID: identity.TunnelID, Lifecycle: connectorprotocol.TunnelDurable, ResourceGeneration: 1, RouteID: id, RouteGeneration: 1, TargetID: "target_" + id, TargetGeneration: 1, HostID: identity.HostID, InstallationGeneration: 1, Audience: "public", ConnectionMethod: "edge", Protocol: protocol, Hostname: host, PathPrefix: path, OriginScheme: protocol, OriginAddress: origin, TLSVerification: "not_applicable", PublicationID: "publication_" + id, PublicationGeneration: 1, ListenerID: listener, PublicPort: publicPort}, DecisionID: "decision_" + id, PolicyGeneration: 1, EdgeNodeID: node.NodeID, EdgeProcessEpoch: node.Epoch, ConnectorID: identity.ConnectorID, SessionID: identity.SessionID, ProcessGeneration: identity.ProcessGeneration, ConfigGeneration: identity.Generation, AssignmentGeneration: 1, Action: "view", IssuedAt: now, ExpiresAt: now.Add(10 * time.Second)}
	}
	var authorities task24Authority
	for i, node := range nodes {
		a := task24Authority{HTTP: decision(node, "http", origins.HTTP), TCP: decision(node, "tcp", origins.TCP)}
		authorities.Nodes = append(authorities.Nodes, a)
		task30Write(t, fixtures[i].AuthorityPath, a)
	}
	task30Write(t, descriptor.AuthorityPath, authorities)
	for _, f := range fixtures {
		var ready bool
		task30Read(t, ctx, f.ReadyPath, &ready)
	}
	for {
		if _, err := os.Stat(descriptor.ReadyPath); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	requestHTTP := func(i int) {
		transport := &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "localhost"}, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(fixtures[i].IP, httpPort))
		}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://app.customer.test/stream", strings.NewReader("task24-upload"))
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != 200 || !bytes.Equal(body, []byte("task24-origin-ok")) {
			t.Fatalf("edge%d HTTP status=%d body=%q", i, response.StatusCode, body)
		}
	}
	requestTCP := func(i int) {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(fixtures[i].IP, tcpPort), 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		io.WriteString(conn, "tcp-upload")
		conn.(*net.TCPConn).CloseWrite()
		body, err := io.ReadAll(conn)
		if err != nil || string(body) != "tcp-delayed-reply" {
			t.Fatalf("edge%d TCP body=%q err=%v", i, body, err)
		}
	}
	requestHTTP(0)
	requestHTTP(1)
	requestTCP(0)
	failedAt := time.Now()
	if err := children[0].Process.Kill(); err != nil {
		t.Fatal(err)
	}
	requestHTTP(1)
	requestTCP(1)
	if elapsed := time.Since(failedAt); elapsed > 20*time.Second {
		t.Fatalf("standby recovery took %v", elapsed)
	}
	t.Logf("two edge processes served the same HTTP hostname/certificate and TCP port; fresh survivor requests recovered in %s", time.Since(failedAt))
	if err := os.WriteFile(descriptor.StopPath, []byte("stop"), 0600); err != nil {
		t.Fatal(err)
	}
}

// task365FileAuthority consumes current SQL-issued decisions without renewing
// them locally. Files are task-owned 0600 fixtures, never production state.
type task365FileAuthority struct{ path string }

func (a task365FileAuthority) Snapshot(ctx context.Context) ([]connectorprotocol.IngressDecision, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	info, err := os.Stat(a.path)
	if err != nil || info.Mode().Perm() != 0600 || info.Size() > task30AuthorityMaximumBytes {
		return nil, connectorprotocol.ErrIngressDenied
	}
	contents, err := os.ReadFile(a.path)
	if err != nil {
		return nil, err
	}
	var authority task24Authority
	if json.Unmarshal(contents, &authority) != nil {
		return nil, connectorprotocol.ErrIngressDenied
	}
	for _, d := range authority.TLS {
		if d.Binding.Protocol != "tls" || d.Validate(time.Now().UTC()) != nil {
			return nil, connectorprotocol.ErrIngressDenied
		}
	}
	return authority.TLS, nil
}
func (a task365FileAuthority) ResolveDecision(ctx context.Context, candidate connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
	decisions, err := a.Snapshot(ctx)
	if err != nil {
		return connectorprotocol.IngressDecision{}, err
	}
	for _, d := range decisions {
		if d.Binding == candidate.Binding && d.AssignmentGeneration == candidate.AssignmentGeneration {
			return d, nil
		}
	}
	return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
}
