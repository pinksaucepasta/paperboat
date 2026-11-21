// Command loadctl is a stand-in control plane for edge capacity runs (tools/loadtest/edge-capacity.md).
//
//	loadctl init  -dir D -control-host H -edge-host H [-preview-domain d] [-tunnel-domain d]
//	loadctl serve -dir D -listen ADDR
//
// init writes everything the real paperboat-edge binary needs (deployment.json, credential, trust files, TLS
// certificates) into D. serve answers the edge's control requests and holds the machine admissions that the load
// generator posts to /admin/admit. It keeps no history and is never a production dependency.
package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
	"github.com/pinksaucepasta/paperboat/shared/tlscertwire"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: loadctl init|serve [flags]")
	}
	var err error
	switch os.Args[1] {
	case "init":
		err = initFiles(os.Args[2:])
	case "serve":
		err = serve(os.Args[2:])
	default:
		err = errors.New("unknown command " + os.Args[1])
	}
	if err != nil {
		log.Fatal(err)
	}
}

func selfSigned(hosts []string, wildcard, ed bool) (certPEM, keyPEM []byte, err error) {
	var key crypto.Signer
	if ed { // the edge's carrier identity must be Ed25519
		_, key, err = ed25519.GenerateKey(rand.Reader)
	} else {
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: hosts[0]},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(72 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, host)
			if wildcard {
				template.DNSNames = append(template.DNSNames, "*."+host)
			}
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

func write(dir, name string, data []byte, mode os.FileMode) error {
	return os.WriteFile(filepath.Join(dir, name), data, mode)
}

func initFiles(args []string) error {
	flags := flag.NewFlagSet("init", flag.ExitOnError)
	dir := flags.String("dir", "", "directory for the edge's files (absolute)")
	controlHost := flags.String("control-host", "127.0.0.1", "address the edge reaches loadctl at")
	controlPort := flags.String("control-port", "8443", "loadctl port")
	edgeHost := flags.String("edge-host", "edge.example.test", "DNS name of the edge (the public certificate must cover it); the load generator resolves it, e.g. through /etc/hosts")
	relayHost := flags.String("relay-host", "relay.example.test", "DNS name in the relay's TLS certificate")
	previewDomain := flags.String("preview-domain", "preview.example.test", "preview base domain")
	tunnelDomain := flags.String("tunnel-domain", "tunnels.example.test", "tunnel base domain")
	runtimeDomain := flags.String("runtime-domain", "runtime.example.test", "runtime base domain")
	extra := flags.String("edge-config-extra", "", "JSON object merged into deployment.json")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !filepath.IsAbs(*dir) {
		return errors.New("-dir must be absolute")
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	controlCert, controlKey, err := selfSigned([]string{*controlHost}, false, false)
	if err != nil {
		return err
	}
	infraCert, infraKey, err := selfSigned([]string{*edgeHost}, false, true)
	if err != nil {
		return err
	}
	publicCert, publicKey, err := selfSigned([]string{*edgeHost, *previewDomain, *tunnelDomain, *runtimeDomain}, true, false)
	if err != nil {
		return err
	}
	relayCert, relayKey, err := selfSigned([]string{*relayHost}, false, false)
	if err != nil {
		return err
	}
	credential := make([]byte, 32)
	if _, err := rand.Read(credential); err != nil {
		return err
	}
	signing, signingPrivate, _ := ed25519.GenerateKey(rand.Reader)
	usageSeed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(usageSeed); err != nil {
		return err
	}
	jwks, _ := json.Marshal(map[string]any{"keys": []map[string]string{{"kty": "OKP", "crv": "Ed25519", "use": "sig", "alg": "EdDSA", "kid": "loadctl", "x": base64.RawURLEncoding.EncodeToString(signing)}}})
	usageKey, _ := json.Marshal(map[string]string{"key_id": "usage_loadctl", "edge_node_id": "edge_load", "private_key": base64.RawURLEncoding.EncodeToString(usageSeed)})
	files := []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{"control.crt", controlCert, 0o644}, {"control.key", controlKey, 0o600}, {"infra.crt", infraCert, 0o644}, {"infra.key", infraKey, 0o600},
		{"public.crt", publicCert, 0o644}, {"public.key", publicKey, 0o600}, {"credential", []byte(base64.RawURLEncoding.EncodeToString(credential)), 0o600},
		{"jwks.json", jwks, 0o644}, {"revocations.json", []byte(`{"jtis":[],"environments":[],"connector_generations":[],"key_ids":[]}`), 0o644},
		{"usage.key", usageKey, 0o600}, {"grant.seed", []byte(base64.RawURLEncoding.EncodeToString(signingPrivate.Seed())), 0o600},
		{"relay.crt", relayCert, 0o644}, {"relay.key", relayKey, 0o600},
	}
	for _, f := range files {
		if err := write(*dir, f.name, f.data, f.mode); err != nil {
			return err
		}
	}
	// The edge container runs as another uid: every file it reads is readable, the usage key keeps its owner-only check by
	// being owned by the container user (docker run --user at the caller).
	base := "/etc/paperboat-edge/"
	deployment := map[string]any{
		"control_url": "https://" + net.JoinHostPort(*controlHost, *controlPort), "credential_issuer": "https://" + net.JoinHostPort(*controlHost, *controlPort),
		"control_credential_file": base + "credential", "control_ca_file": base + "control.crt", "jwks_file": base + "jwks.json",
		"revocations_file": base + "revocations.json", "usage_signing_key_file": base + "usage.key",
		"infrastructure_tls_cert_file": base + "infra.crt", "infrastructure_tls_key_file": base + "infra.key",
		"public_https_cert_file": base + "public.crt", "public_https_key_file": base + "public.key",
		"connector_advertise_host": *edgeHost, "carrier_tcp_listen_address": "0.0.0.0:7272", "carrier_quic_listen_address": "0.0.0.0:7273",
		"public_https_listen_address": "0.0.0.0:443", "private_https_listen_address": "127.0.0.1:7279", "public_http_listen_address": "0.0.0.0:80",
		"preview_base_domain": *previewDomain, "tunnel_base_domain": *tunnelDomain, "runtime_base_domain": *runtimeDomain,
		"trusted_proxy_cidrs": []string{},
		"control_interval":    int64(5 * time.Second), "control_timeout": int64(5 * time.Second),
	}
	if *extra != "" {
		var merged map[string]any
		if err := json.Unmarshal([]byte(*extra), &merged); err != nil {
			return err
		}
		for k, v := range merged {
			deployment[k] = v
		}
	}
	data, _ := json.MarshalIndent(deployment, "", "  ")
	return write(*dir, "deployment.json", data, 0o644)
}

type entry struct {
	adm     runtimewire.Admission
	version uint64
}

type controller struct {
	credential string
	dir        string
	wildcards  []struct{ id, name string } // the domains the wildcard certificate is distributed for

	mu           sync.Mutex
	changed      chan struct{} // closed and replaced when the endpoint set changes
	registration *control.NodeRegistration
	runtime      map[string]runtimewire.Admission
	endpoints    map[string]*entry
	version      uint64
	ready        map[string]bool
	acked        map[string]bool // certificate id/action the edge confirmed
	relay        map[string]any  // the relay node's generation and epoch, for the load generator's grants
	observed     atomic.Uint64
	requests     sync.Map // path -> *atomic.Uint64
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	dir := flags.String("dir", "", "directory written by init")
	listen := flags.String("listen", "0.0.0.0:8443", "listen address")
	previewDomain := flags.String("preview-domain", "preview.example.test", "preview base domain")
	tunnelDomain := flags.String("tunnel-domain", "tunnels.example.test", "tunnel base domain")
	if err := flags.Parse(args); err != nil {
		return err
	}
	credential, err := os.ReadFile(filepath.Join(*dir, "credential"))
	if err != nil {
		return err
	}
	certificate, err := tls.LoadX509KeyPair(filepath.Join(*dir, "control.crt"), filepath.Join(*dir, "control.key"))
	if err != nil {
		return err
	}
	c := &controller{credential: string(credential), dir: *dir, changed: make(chan struct{}), runtime: map[string]runtimewire.Admission{}, endpoints: map[string]*entry{}, ready: map[string]bool{}, acked: map[string]bool{}, relay: map[string]any{}}
	c.wildcards = []struct{ id, name string }{{"preview", *previewDomain}, {"tunnel", *tunnelDomain}}
	server := &http.Server{Addr: *listen, Handler: http.HandlerFunc(c.route), TLSConfig: &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13}}
	log.Printf("loadctl listening on %s", *listen)
	return server.ListenAndServeTLS("", "")
}

func (c *controller) count(path string) {
	value, _ := c.requests.LoadOrStore(path, new(atomic.Uint64))
	value.(*atomic.Uint64).Add(1)
}

func (c *controller) authorized(r *http.Request) bool {
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+c.credential)) == 1
}

func reply(w http.ResponseWriter, status int, body any) {
	if body == nil {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (c *controller) route(w http.ResponseWriter, r *http.Request) {
	if !c.authorized(r) {
		reply(w, http.StatusUnauthorized, nil)
		return
	}
	c.count(r.URL.Path)
	switch r.URL.Path {
	case "/v1/nodes/register":
		var registration control.NodeRegistration
		if json.NewDecoder(r.Body).Decode(&registration) != nil {
			reply(w, http.StatusBadRequest, nil)
			return
		}
		c.mu.Lock()
		c.registration = &registration
		c.mu.Unlock()
		reply(w, http.StatusNoContent, nil)
	case "/v1/nodes/heartbeat":
		reply(w, http.StatusOK, map[string]any{})
	case "/v1/trust/jwks":
		c.file(w, "jwks.json")
	case "/v1/trust/revocations":
		c.file(w, "revocations.json")
	case "/v1/edge/runtime/carrier-admissions":
		c.runtimePull(w)
	case "/v1/edge/runtime/endpoint-admissions":
		c.endpointPull(w, r)
	case "/v1/edge/runtime/carrier-observations":
		c.observe(w, r)
	case "/v1/edge/routes/desired-state":
		reply(w, http.StatusOK, map[string]any{"complete": true, "version": "v1", "routes": []any{}})
	case "/v1/edge/routes/observations":
		reply(w, http.StatusNoContent, nil)
	case "/v1/edge/ingress/desired-state":
		c.ingress(w, r)
	case tlscertwire.PullPath:
		c.distribution(w)
	case tlscertwire.AckPath:
		var ack tlscertwire.DistributionAck
		if json.NewDecoder(r.Body).Decode(&ack) == nil && (ack.Status == "ready" || ack.Status == "active") {
			c.mu.Lock()
			c.acked[ack.CertificateID+"/"+ack.Action] = true
			c.mu.Unlock()
		}
		reply(w, http.StatusNoContent, nil)
	case control.PrivateAccessCarrierAdmissionsPath:
		reply(w, http.StatusOK, connectorprotocol.PrivateAccessSnapshot{Schema: connectorprotocol.PrivateAccessSchema, Kind: connectorprotocol.PrivateAccessSnapshotKind, Complete: true, Admissions: []connectorprotocol.PrivateAccessAdmission{}})
	case "/v1/edge/usage-reports":
		reply(w, http.StatusOK, map[string]string{"disposition": "accounted"})
	case "/v1/relay/nodes/start", "/v1/relay/nodes/observe":
		c.relayLease(w, r)
	case "/admin/relay":
		c.mu.Lock()
		defer c.mu.Unlock()
		reply(w, http.StatusOK, c.relay)
	case "/admin/registration":
		c.mu.Lock()
		defer c.mu.Unlock()
		reply(w, http.StatusOK, c.registration)
	case "/admin/admit":
		c.admit(w, r)
	case "/admin/reset":
		c.mu.Lock()
		c.runtime, c.endpoints, c.ready = map[string]runtimewire.Admission{}, map[string]*entry{}, map[string]bool{}
		c.bump()
		c.mu.Unlock()
		reply(w, http.StatusNoContent, nil)
	case "/admin/stats":
		c.stats(w)
	default:
		log.Printf("unhandled %s %s", r.Method, r.URL.Path)
		reply(w, http.StatusNotFound, nil)
	}
}

// distribution hands the edge the platform wildcard certificate (public.crt) for the preview and tunnel domains, as the server
// does: a stage and an activate message each, repeated until the edge acknowledges them.
func (c *controller) distribution(w http.ResponseWriter) {
	c.mu.Lock()
	registration := c.registration
	acked := map[string]bool{}
	for k := range c.acked {
		acked[k] = true
	}
	c.mu.Unlock()
	page := tlscertwire.DistributionPage{Version: 1, Complete: true, Messages: []tlscertwire.DistributionMessage{}}
	if registration == nil {
		reply(w, http.StatusOK, page)
		return
	}
	certPEM, err1 := os.ReadFile(filepath.Join(c.dir, "public.crt"))
	keyPEM, err2 := os.ReadFile(filepath.Join(c.dir, "public.key"))
	block, _ := pem.Decode(certPEM)
	if err1 != nil || err2 != nil || block == nil {
		reply(w, http.StatusInternalServerError, nil)
		return
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		reply(w, http.StatusInternalServerError, nil)
		return
	}
	fingerprint := sha256.Sum256(leaf.Raw)
	now := time.Now().UTC()
	for _, domain := range c.wildcards {
		for _, action := range []string{"stage", "activate"} {
			id := "certificate_" + domain.id
			if acked[id+"/"+action] {
				continue
			}
			m := tlscertwire.DistributionMessage{Version: 1, Action: action, CertificateID: id, AccountID: "account_platform", DomainID: "domain_" + domain.id,
				TargetKind: string(tlscertwire.TargetPlatformWildcard), Hostname: "*." + domain.name, DomainGeneration: 1, CertificateGeneration: 1,
				EdgeNodeID: registration.NodeID, EdgeProcessEpoch: registration.ProcessEpoch, AssignmentGeneration: 1,
				Fingerprint: base64.RawURLEncoding.EncodeToString(fingerprint[:]), Issuer: "loadctl", NotBefore: leaf.NotBefore, ExpiresAt: now.Add(5 * time.Minute),
				CertificateExpiresAt: leaf.NotAfter, CertificatePEM: certPEM, PrivateKeyPEM: keyPEM, IssuedAt: now}
			if action != "stage" { // the edge's verifier rebuilds the message without the bundle for every other action
				m.CertificatePEM, m.PrivateKeyPEM = nil, nil
			}
			proof, err := tlscertwire.ProofMAC(m, []byte(c.credential))
			if err != nil {
				reply(w, http.StatusInternalServerError, nil)
				return
			}
			m.Proof = base64.RawURLEncoding.EncodeToString(proof)
			page.Messages = append(page.Messages, m)
		}
	}
	reply(w, http.StatusOK, page)
}

// relayLease answers a relay's start and observe requests the way the control plane does: the node's generation and epoch, a
// short lease, the node's connection capacity, the issuer trust document and the speed every account is held to.
func (c *controller) relayLease(w http.ResponseWriter, r *http.Request) {
	var request struct {
		NodeID     string `json:"node_id"`
		Expected   uint64 `json:"expected_generation"`
		Generation uint64 `json:"node_generation"`
		Epoch      string `json:"process_epoch"`
	}
	if json.NewDecoder(r.Body).Decode(&request) != nil || request.NodeID == "" {
		reply(w, http.StatusBadRequest, nil)
		return
	}
	generation := request.Generation
	if r.URL.Path == "/v1/relay/nodes/start" {
		generation = request.Expected + 1
	}
	jwks, _ := os.ReadFile(filepath.Join(c.dir, "jwks.json"))
	c.mu.Lock()
	c.relay = map[string]any{"node_id": request.NodeID, "generation": generation, "epoch": request.Epoch}
	c.mu.Unlock()
	reply(w, http.StatusOK, map[string]any{"node_id": request.NodeID, "node_generation": generation, "process_epoch": request.Epoch,
		"expires_at": time.Now().Add(15 * time.Second).Unix(), "capacity_limit": 16384, "revocations": []any{}, "account_mbit": 10.0, "jwks": json.RawMessage(jwks)})
}

func (c *controller) file(w http.ResponseWriter, name string) {
	data, err := os.ReadFile(filepath.Join(c.dir, name))
	if err != nil {
		reply(w, http.StatusInternalServerError, nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// bump wakes held endpoint pulls; the caller holds c.mu.
func (c *controller) bump() {
	c.version++
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *controller) admit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Runtime   []runtimewire.Admission `json:"runtime"`
		Endpoints []runtimewire.Admission `json:"endpoints"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		reply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range body.Runtime {
		c.runtime[a.Binding.RouteID] = a
	}
	c.version++
	for _, a := range body.Endpoints {
		c.endpoints[a.Binding.RouteID] = &entry{adm: a, version: c.version}
	}
	close(c.changed)
	c.changed = make(chan struct{})
	reply(w, http.StatusNoContent, nil)
}

const admissionLifetime = 10 * time.Minute

func (c *controller) runtimePull(w http.ResponseWriter) {
	c.mu.Lock()
	out := make([]runtimewire.Admission, 0, len(c.runtime))
	expires := time.Now().UTC().Add(admissionLifetime)
	for _, a := range c.runtime {
		a.ExpiresAt = expires
		out = append(out, a)
	}
	c.mu.Unlock()
	reply(w, http.StatusOK, runtimewire.Snapshot{Schema: runtimewire.Schema, Complete: true, Admissions: &out})
}

func (c *controller) endpointPull(w http.ResponseWriter, r *http.Request) {
	var request runtimewire.EdgeRequest
	if json.NewDecoder(r.Body).Decode(&request) != nil {
		reply(w, http.StatusBadRequest, nil)
		return
	}
	since, _ := strconv.ParseUint(request.Since, 10, 64)
	if request.Since != "" && request.WaitMilliseconds > 0 {
		deadline := time.NewTimer(time.Duration(request.WaitMilliseconds) * time.Millisecond)
		defer deadline.Stop()
		for {
			c.mu.Lock()
			current, wake := c.version, c.changed
			c.mu.Unlock()
			if current > since {
				break
			}
			select {
			case <-wake:
				continue
			case <-deadline.C:
			case <-r.Context().Done():
			}
			break
		}
	}
	c.mu.Lock()
	cursor := strconv.FormatUint(c.version, 10)
	var admissions []runtimewire.Admission
	expires := time.Now().UTC().Add(admissionLifetime)
	for _, e := range c.endpoints {
		if request.Since == "" || e.version > since {
			a := e.adm
			a.ExpiresAt = expires
			admissions = append(admissions, a)
		}
	}
	c.mu.Unlock()
	carriers := runtimewire.GroupEndpointAdmissions(admissions)
	if carriers == nil {
		carriers = []runtimewire.EndpointCarrier{}
	}
	snapshot := runtimewire.Snapshot{Schema: runtimewire.Schema, Complete: request.Since == "", Carriers: &carriers, Cursor: cursor}
	if !snapshot.Complete {
		snapshot.ExpiresAt = &expires
	}
	reply(w, http.StatusOK, snapshot)
}

func (c *controller) observe(w http.ResponseWriter, r *http.Request) {
	var request runtimewire.EdgeRequest
	if json.NewDecoder(r.Body).Decode(&request) != nil {
		reply(w, http.StatusBadRequest, nil)
		return
	}
	c.mu.Lock()
	for _, o := range request.Observations {
		c.ready[o.RouteID] = o.Ready
	}
	c.mu.Unlock()
	c.observed.Add(uint64(len(request.Observations)))
	reply(w, http.StatusNoContent, nil)
}

func (c *controller) ingress(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Version string `json:"version"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	c.mu.Lock()
	version := "v" + strconv.FormatUint(c.version, 10)
	entries := make([]*entry, 0, len(c.endpoints))
	for _, e := range c.endpoints {
		entries = append(entries, e)
	}
	c.mu.Unlock()
	now := time.Now().UTC()
	issued, expires := now.Add(-time.Second), now.Add(connectorprotocol.IngressAuthorityLifetime-time.Second)
	if request.Version == version {
		expiries := make([]time.Time, len(entries))
		for i := range expiries {
			expiries[i] = expires
		}
		reply(w, http.StatusOK, map[string]any{"version": version, "unchanged": true, "complete": true, "issued_at": now, "expires_at": expiries})
		return
	}
	decisions := make([]connectorprotocol.IngressDecision, 0, len(entries))
	for _, e := range entries {
		decisions = append(decisions, decision(e.adm, issued, expires))
	}
	reply(w, http.StatusOK, map[string]any{"version": version, "complete": true, "issued_at": now, "decisions": decisions,
		"throttled": []any{}, "blocked": []any{}, "ip_rules": []any{}, "offline": []any{}, "http01_responses": []any{}})
}

func decision(a runtimewire.Admission, issued, expires time.Time) connectorprotocol.IngressDecision {
	b := a.Binding
	return connectorprotocol.IngressDecision{Binding: connectorprotocol.IngressBinding{EnvironmentID: "environment_load", AccountID: b.AccountID, TunnelID: b.TunnelID,
		Lifecycle: connectorprotocol.TunnelEphemeral, ResourceGeneration: 1, RouteID: b.RouteID, RouteGeneration: b.RouteGeneration, TargetID: b.RouteID, TargetGeneration: b.RouteGeneration,
		HostID: b.HostID, InstallationGeneration: 1, Audience: "public", ConnectionMethod: "edge", Protocol: "http", Hostname: a.Hostname, PathPrefix: "/",
		OriginScheme: "http", OriginAddress: "127.0.0.1:3000", TLSVerification: "not_applicable", PublicationID: a.EndpointID, PublicationGeneration: 1},
		DecisionID: "decision_" + a.EndpointID, PolicyGeneration: 1, EdgeNodeID: b.EdgeNodeID, EdgeProcessEpoch: b.EdgeProcessEpoch,
		ConnectorID: b.ConnectorID, SessionID: b.SessionID, ProcessGeneration: b.ProcessGeneration, ConfigGeneration: b.ConfigGeneration,
		AssignmentGeneration: a.AttachmentGeneration, Action: "view", IssuedAt: issued, ExpiresAt: expires}
}

func (c *controller) stats(w http.ResponseWriter) {
	c.mu.Lock()
	ready := 0
	for _, ok := range c.ready {
		if ok {
			ready++
		}
	}
	out := map[string]any{"runtime": len(c.runtime), "endpoints": len(c.endpoints), "ready_routes": ready, "observations": c.observed.Load(), "registered": c.registration != nil}
	c.mu.Unlock()
	requests := map[string]uint64{}
	c.requests.Range(func(k, v any) bool { requests[k.(string)] = v.(*atomic.Uint64).Load(); return true })
	out["requests"] = requests
	reply(w, http.StatusOK, out)
}

var _ = context.Background
var _ = fmt.Sprintf
