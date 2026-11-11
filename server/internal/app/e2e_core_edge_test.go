package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/miekg/dns"
)

// The edge half of TestE2ECore: a self-hosted installation (relay/selfhost, the supervisor pbh runs) with the real
// paperboat-tunnel and paperboat-relay, claimed by the operator as a managed edge, so previews, tunnels, sends and
// relayed native traffic go through it; certificates come from Pebble (Let's Encrypt's test CA, validation off).

const e2ePebbleImage = "ghcr.io/letsencrypt/pebble:2.7.0"

// buildEdge builds the edge and relay binaries and the installation supervisor (relay/selfhost TestE2ESelfhostNode).
func (e *e2eCore) buildEdge() error {
	bin := filepath.Join(e.work, "bin")
	for _, build := range [][]string{
		{"tunnel", "build", "-trimpath", "-o", filepath.Join(bin, "paperboat-tunnel"), "./cmd/paperboat-tunnel"},
		{"relay", "build", "-trimpath", "-o", filepath.Join(bin, "paperboat-relay"), "./cmd/paperboat-relay"},
		{"relay", "test", "-c", "-o", filepath.Join(e.work, "selfhost-node.test"), "./selfhost"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		cmd := exec.CommandContext(ctx, "go", build[1:]...)
		cmd.Dir, cmd.Env = filepath.Join(e.root, build[0]), append(os.Environ(), "CGO_ENABLED=0")
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			return fmt.Errorf("go %s in %s: %w: %s", build[1], build[0], err, lastBytes(string(out), 2000))
		}
	}
	return nil
}

// startPebble runs Pebble with validation off and keeps its TLS root for the server's ACME client.
func (e *e2eCore) startPebble() error {
	_, _ = e.run(time.Minute, "docker", "rm", "-f", "e2e-pebble")
	if _, err := e.run(5*time.Minute, "docker", "run", "-d", "--name", "e2e-pebble", "-p", "127.0.0.1:14000:14000", "-p", "127.0.0.1:15000:15000",
		"-e", "PEBBLE_VA_ALWAYS_VALID=1", "-e", "PEBBLE_VA_NOSLEEP=1", e2ePebbleImage); err != nil {
		return err
	}
	if _, err := e.run(time.Minute, "docker", "cp", "e2e-pebble:/test/certs/pebble.minica.pem", filepath.Join(e.work, "pebble.pem")); err != nil {
		return err
	}
	client, err := e.clientTrusting(filepath.Join(e.work, "pebble.pem"), nil)
	if err != nil {
		return err
	}
	return waitFor(time.Minute, func() error { return getOK(client, "https://localhost:14000/dir") })
}

// startCAAResolver answers every DNS question with NXDOMAIN: no CAA record, so any issuer may issue.
func (e *e2eCore) startCAAResolver() error {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	server := &dns.Server{PacketConn: conn, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetRcode(r, dns.RcodeNameError)
		_ = w.WriteMsg(m)
	})}
	go func() { _ = server.ActivateAndServe() }()
	e.t.Cleanup(func() { _ = server.Shutdown() })
	e.caaResolver = conn.LocalAddr().String()
	return nil
}

// startEdge runs the installation supervisor; the first start writes claim.json, a restart serves the claimed edge.
func (e *e2eCore) startEdge() error {
	config, err := json.Marshal(map[string]any{
		"name": "e2e-edge", "endpoint_host": e.gateway, "listen": net.JoinHostPort(e.gateway, "18443"),
		"components": []map[string]any{
			{"capability": "tunnel", "tcp_port": 27443, "quic_port": 27444, "region": "e2e", "failure_domain": "e2e", "capacity_limit": 128},
			{"capability": "relay", "tcp_port": 27445, "quic_port": 27446, "region": "e2e", "failure_domain": "e2e", "capacity_limit": 128},
		},
		"preview_domain": "public.example.test", "tunnel_domain": "public.example.test", "runtime_domain": "runtime.example.test",
		"control_ca":             filepath.Join(e.work, "ca.pem"),
		"public_https_cert_file": filepath.Join(e.work, "tls.pem"),
		"public_https_key_file":  filepath.Join(e.work, "tls.key"),
	})
	if err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(e.work, "edge.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(filepath.Join(e.work, "selfhost-node.test"), "-test.run", "^TestE2ESelfhostNode$", "-test.v", "-test.timeout", "30m")
	cmd.Env = append(os.Environ(), "PAPERBOAT_E2E_SELFHOST_DIR="+filepath.Join(e.work, "edge"),
		"PAPERBOAT_E2E_SELFHOST_BINARIES="+filepath.Join(e.work, "bin"), "PAPERBOAT_E2E_SELFHOST_CONFIG="+string(config))
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	e.edge, e.edgeDone = cmd, done
	return nil
}

func (e *e2eCore) stopEdge() error {
	if e.edge == nil {
		return nil
	}
	_ = e.edge.Process.Signal(syscall.SIGTERM)
	defer func() { e.edge = nil }()
	select {
	case err := <-e.edgeDone:
		return err
	case <-time.After(60 * time.Second):
		_ = e.edge.Process.Kill()
		<-e.edgeDone // reap it: its exit releases the ports
		return errors.New("edge did not stop within 60 s of SIGTERM")
	}
}

// claimEdge enrolls the installation as the operator (Dashboard → Network → Add node) and waits until both of its
// components report ready to the server.
func (e *e2eCore) claimEdge() error {
	var claim struct{ Code, Address string }
	if err := waitFor(time.Minute, func() error {
		raw, err := os.ReadFile(filepath.Join(e.work, "edge", "claim.json"))
		if err == nil {
			err = json.Unmarshal(raw, &claim)
		}
		return err
	}); err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"address": claim.Address, "code": claim.Code})
	if _, err := e.operator(http.MethodPost, "/v1/admin/selfhost/enrollments", string(body), claim.Address); err != nil {
		return err
	}
	return e.awaitEdgeReady()
}

func (e *e2eCore) awaitEdgeReady() error {
	return waitFor(2*time.Minute, func() error {
		raw, err := e.operator(http.MethodGet, "/v1/admin/selfhost/installations", "", "")
		if err != nil {
			return err
		}
		var out struct {
			Data struct {
				Installations []struct {
					Capability string
					Ready      bool
				}
			}
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return err
		}
		ready := 0
		for _, installation := range out.Data.Installations {
			if installation.Ready {
				ready++
			}
		}
		if len(out.Data.Installations) != 2 || ready != 2 {
			return fmt.Errorf("edge components: %s", lastBytes(string(raw), 400))
		}
		return nil
	})
}

// awaitEdgeServes waits until the edge completes a TLS handshake for a runtime name, which needs the platform
// wildcard certificate the server distributes to a new edge process (held in memory only). "Ready" alone says
// the node reported in, not that it can serve; the server's dispatch gives a handshake 10 s. Tailscale's
// tstest/integration AwaitRunning gates on the data path the same way.
func (e *e2eCore) awaitEdgeServes() error {
	raw, err := os.ReadFile(filepath.Join(e.work, "pebble-roots.pem"))
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(raw)
	started := time.Now()
	err = waitFor(2*time.Minute, func() error {
		dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 3 * time.Second}, Config: &tls.Config{RootCAs: roots, ServerName: "probe.runtime.example.test"}}
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(e.gateway, "443"))
		if err == nil {
			_ = conn.Close()
		}
		return err
	})
	e.t.Logf("edge served a TLS handshake %s after it was ready (err=%v)", time.Since(started).Round(time.Millisecond), err)
	return err
}

// operator calls the server as the seeded operator through the TLS front; a write confirms its target, as the
// operator console does (Operator-Target, Operator-Reason, Idempotency-Key).
func (e *e2eCore) operator(method, path, body, target string) ([]byte, error) {
	request, err := http.NewRequest(method, "https://"+e.host+":"+e.port+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.AddCookie(&http.Cookie{Name: "pb-operator-session", Value: e2eOperatorSession})
	request.Header.Set("X-CSRF-Token", e2eOperatorCSRF)
	request.Header.Set("Content-Type", "application/json")
	if method != http.MethodGet {
		request.Header.Set("Operator-Target", target)
		request.Header.Set("Operator-Reason", "e2e-core")
		request.Header.Set("Idempotency-Key", "e2e-core-"+strings.NewReplacer(":", "-", ".", "-").Replace(target))
	}
	response, err := e.serverClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode >= 300 {
		return raw, fmt.Errorf("%s %s: %s: %s", method, path, response.Status, lastBytes(string(raw), 400))
	}
	return raw, nil
}

// assertEdge is every path through the edge: a file send (the runtime route), a public preview and a tunnel
// fetched through the edge's HTTPS listener from outside.
func (e *e2eCore) assertEdge(phase string) {
	a, b := e.machines[0], e.machines[1]
	e.step(fmt.Sprintf("%s: send %s -> %s", phase, a, b), func() error {
		content := "send-ok " + phase
		file := "/home/pbt/e2e-" + strings.ReplaceAll(phase, " ", "-") + ".txt"
		if _, err := e.docker(a, "bash", "-c", fmt.Sprintf("printf %%s %q >%s", content, file)); err != nil {
			return err
		}
		if err := waitFor(90*time.Second, func() error { _, err := e.pb(a, "send", file, "--to", b); return err }); err != nil {
			return err
		}
		inbox, err := e.pb(b, "inbox", "path")
		if err != nil {
			return err
		}
		return waitFor(30*time.Second, func() error { _, err := e.docker(b, "grep", "-rqF", content, strings.TrimSpace(inbox)); return err })
	})
	e.step(fmt.Sprintf("%s: preview of %s fetched through the edge", phase, b), func() error {
		return e.publication(phase, b, "preview", "8080", "--background", "--ttl", "30m", "--json")
	})
	e.step(fmt.Sprintf("%s: tunnel of %s fetched through the edge", phase, b), func() error {
		return e.publication(phase, b, "tunnel", "8081", "--ephemeral", "--background", "--json")
	})
}

// publication serves a marker over HTTP on port in machine, publishes it with pb <kind> and fetches it through
// the edge's public HTTPS listener with the name the server assigned.
func (e *e2eCore) publication(phase, machine, kind, port string, args ...string) error {
	marker := kind + "-ok " + phase
	serve := fmt.Sprintf("mkdir -p /tmp/%[1]s && printf %%s %[2]q >/tmp/%[1]s/index.html && cd /tmp/%[1]s && (setsid python3 -m http.server %[1]s >/dev/null 2>&1 &)", port, marker)
	if _, err := e.run(time.Minute, "docker", "exec", "-u", "pbt", machine, "bash", "-c", serve); err != nil {
		return err
	}
	out, err := e.pb(machine, append([]string{kind, port}, args...)...)
	if err != nil {
		return err
	}
	hostname := publicHostname(out)
	if hostname == "" {
		return fmt.Errorf("pb %s printed no public URL: %s", kind, lastBytes(out, 400))
	}
	client, err := e.clientTrusting(filepath.Join(e.work, "pebble-roots.pem"), func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(e.gateway, "443"))
	})
	if err != nil {
		return err
	}
	return waitFor(2*time.Minute, func() error {
		response, err := client.Get("https://" + hostname + "/")
		if err != nil {
			return err
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		if response.StatusCode != http.StatusOK || !strings.Contains(string(raw), marker) {
			return fmt.Errorf("GET https://%s/: %s: %s", hostname, response.Status, lastBytes(string(raw), 200))
		}
		return nil
	})
}

// publicHostname finds the first https URL under the test domains in pb's JSON output.
func publicHostname(out string) string {
	for _, field := range strings.FieldsFunc(out, func(r rune) bool { return r == '"' || r == ' ' || r == '\n' }) {
		if host, ok := strings.CutPrefix(field, "https://"); ok && (strings.Contains(host, ".public.example.test")) {
			return strings.TrimRight(strings.SplitN(host, "/", 2)[0], ",")
		}
	}
	return ""
}

// pebbleRoots saves the root Pebble issues leaf certificates under (it changes every Pebble start).
func (e *e2eCore) pebbleRoots() error {
	client, err := e.clientTrusting(filepath.Join(e.work, "pebble.pem"), nil)
	if err != nil {
		return err
	}
	response, err := client.Get("https://localhost:15000/roots/0")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("pebble roots: %s", response.Status)
	}
	if err := os.WriteFile(filepath.Join(e.work, "pebble-roots.pem"), raw, 0o600); err != nil {
		return err
	}
	// Trust both Pebble's API and issuance roots, plus the fixture's public ingress CA.
	var roots []byte
	for _, name := range []string{"pebble.pem", "pebble-roots.pem", "ca.pem"} {
		pem, err := os.ReadFile(filepath.Join(e.work, name))
		if err != nil {
			return err
		}
		roots = append(append(roots, pem...), '\n')
	}
	return os.WriteFile(filepath.Join(e.work, "server-roots.pem"), roots, 0o600)
}

// blockDirect drops traffic between the two machines, so native connections must go through the relay.
func (e *e2eCore) blockDirect() error {
	addresses := map[string]string{}
	for _, name := range e.machines {
		out, err := e.run(time.Minute, "docker", "inspect", "-f", "{{.NetworkSettings.IPAddress}}", name)
		if err != nil {
			return err
		}
		addresses[name] = strings.TrimSpace(out)
	}
	a, b := e.machines[0], e.machines[1]
	for machine, other := range map[string]string{a: addresses[b], b: addresses[a]} {
		if _, err := e.docker(machine, "iptables", "-I", "OUTPUT", "-d", other, "-j", "DROP"); err != nil {
			return err
		}
	}
	return nil
}

// clientTrusting is an HTTPS client that trusts only the roots in pemFile, optionally dialing through dial.
func (e *e2eCore) clientTrusting(pemFile string, dial func(context.Context, string, string) (net.Conn, error)) (*http.Client, error) {
	raw, err := os.ReadFile(pemFile)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(raw) {
		return nil, fmt.Errorf("%s holds no certificate", pemFile)
	}
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}, DialContext: dial}}, nil
}

func getOK(client *http.Client, url string) error {
	response, err := client.Get(url)
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, response.Status)
	}
	return nil
}
