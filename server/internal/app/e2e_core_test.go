package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// TestE2ECore is the core end-to-end check (e2e.yml "End-to-end core"; before a push: tools/ci-test.sh e2e).
// The real server (this test binary run again as TestE2EServe, on PostgreSQL, behind TLS) and two real `pb`
// daemons, each in its own systemd container with its own state, paired like a user pairs them. It asserts
// exec both ways and managed SSH with a command; then restarts the server like a deploy (SIGTERM, start again
// on the same database) and asserts all of it again. (A file send needs the edge's runtime route.) A failure names the step and prints the
// server's and both daemons' logs.
//
// Prior art: Tailscale tstest/integration/integration.go (GetBinaries/build: real binaries built once;
// TestEnv runs control; TestNode.StartDaemon and AwaitRunning drive real daemons; tstest.WaitFor bounds every
// wait) and headscale integration/tsic/tsic.go + dockertestutil/execute.go (real clients in docker containers,
// commands run with docker exec under a bound). Its server and edge children stop with it (Pdeathsig); after a
// killed run, `docker rm -f e2e-a e2e-b e2e-pebble` removes what it started.
func TestE2ECore(t *testing.T) {
	if os.Getenv("PAPERBOAT_E2E_CORE") == "" {
		t.Skip("core end-to-end only: e2e.yml End-to-end core (PAPERBOAT_E2E_CORE=1, docker, PostgreSQL)")
	}
	if err := db.ValidateIsolatedTestDSN(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"), os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	port, internal := freePort(t, "0.0.0.0"), freePort(t, "127.0.0.1")
	for internal == port {
		internal = freePort(t, "127.0.0.1")
	}
	e := &e2eCore{t: t, root: root, work: t.TempDir(), host: "e2e-server.example.test", port: port,
		internal: "127.0.0.1:" + internal, mintKey: "e2e:" + base64.RawURLEncoding.EncodeToString(seed), machines: []string{"e2e-a", "e2e-b"}}
	t.Cleanup(e.cleanup)

	e.step("host", e.checkHost)
	e.step("certificates", e.certificates)
	e.step("build pb", e.buildPB)
	e.step("build edge and relay", e.buildEdge)
	e.step("pebble", e.startPebble)
	e.step("pebble roots", e.pebbleRoots)
	e.step("CAA resolver", e.startCAAResolver)
	e.step("server start", func() error { return e.startServer(true) })
	e.step("edge start", e.startEdge)
	e.step("edge claim", e.claimEdge)
	e.step("machine image", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		build := exec.CommandContext(ctx, "docker", "build", "--progress=plain", "--network=host", "-t", "paperboat-e2e-machine", "-f", "tools/e2e/machine.Dockerfile", "tools/e2e")
		build.Dir = e.root // --network=host: the runner's resolver (GitHub's docker build network resolves no names)
		out, err := build.CombinedOutput()
		if err != nil {
			return fmt.Errorf("docker build: %w: %s", err, lastBytes(string(out), 4000))
		}
		return nil
	})
	for _, name := range e.machines {
		e.step("machine "+name, func() error { return e.startMachine(name) })
		e.step("pair "+name, func() error { return e.pair(name) })
	}
	e.assertAll("first run")
	e.step("server restart (deploy)", e.restartServer)
	e.assertAll("after server restart")
	e.step("edge restart (deploy)", func() error {
		if err := e.stopEdge(); err != nil {
			return err
		}
		if err := e.startEdge(); err != nil {
			return err
		}
		if err := e.awaitEdgeReady(); err != nil {
			return err
		}
		if err := e.awaitEdgeServes(); err != nil {
			return err
		}
		return e.requireRestoredCertificates()
	})
	e.assertAll("after edge restart")
	e.step("block the direct path between the machines", e.blockDirect)
	e.assertCore("relayed")
}

func (e *e2eCore) assertAll(phase string) {
	e.assertCore(phase)
	e.assertEdge(phase)
}

// checkHost finds the docker bridge gateway (where the machines reach the server and the edge) and checks the
// host resolves the server's name, which the edge's control client dials (CI adds it to /etc/hosts).
func (e *e2eCore) checkHost() error {
	gateway, err := e.run(time.Minute, "docker", "network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}")
	if err != nil {
		return err
	}
	e.gateway = strings.TrimSpace(gateway)
	if addresses, err := net.LookupHost(e.host); err != nil || len(addresses) == 0 {
		return fmt.Errorf("%s does not resolve on this host (add \"127.0.0.1 %s\" to /etc/hosts): %v", e.host, e.host, err)
	}
	return nil
}

type e2eCore struct {
	t          *testing.T
	root, work string
	host, port string
	internal   string // the server's plain HTTP address behind the TLS front
	mintKey    string // id:seed, kept across server restarts like production's
	machines   []string
	gateway    string // the docker bridge gateway: the host as the machines see it; the edge listens there
	server     *exec.Cmd
	serverDone chan error
	// serverClient reaches the server through its TLS front with the test CA.
	serverClient *http.Client
	caaResolver  string
	edge         *exec.Cmd
	edgeDone     chan error
}

// assertCore is every core path, each its own named step. The first command of a phase waits (bounded) for
// the daemons to reach the server and each other; everything after it must work at once.
func (e *e2eCore) assertCore(phase string) {
	a, b := e.machines[0], e.machines[1]
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		from, to := pair[0], pair[1]
		e.step(fmt.Sprintf("%s: exec %s -> %s", phase, from, to), func() error {
			want := "exec-ok-" + to
			return waitFor(90*time.Second, func() error { return e.pbWant(want, from, "exec", to, "--", "echo", want) })
		})
	}
	e.step(fmt.Sprintf("%s: ssh %s -> %s with a command", phase, a, b), func() error {
		return waitFor(60*time.Second, func() error {
			return e.pbWant("ssh-ok", a, "ssh", b, "--", "-o", "BatchMode=yes", "echo", "ssh-ok")
		})
	})
}

// step runs one named step; a failure prints the logs and stops the test with the step's name.
func (e *e2eCore) step(name string, fn func() error) {
	e.t.Helper()
	started := time.Now()
	if err := fn(); err != nil {
		e.diagnose()
		e.t.Fatalf("step %q failed after %s: %v", name, time.Since(started).Round(time.Millisecond), err)
	}
	e.t.Logf("step %q ok (%s)", name, time.Since(started).Round(time.Millisecond))
}

// waitFor retries try until it succeeds or the bound passes, as tstest.WaitFor does; the last error is kept.
func waitFor(bound time.Duration, try func() error) error {
	deadline := time.Now().Add(bound)
	for delay := 250 * time.Millisecond; ; delay = min(2*delay, 3*time.Second) {
		err := try()
		var stop backoffStop
		if err == nil || errors.As(err, &stop) {
			return err
		}
		if time.Now().Add(delay).After(deadline) {
			return fmt.Errorf("not within %s: %w", bound, err)
		}
		time.Sleep(delay)
	}
}

// pbWant runs pb in a machine and wants want as the last line of its output.
func (e *e2eCore) pbWant(want, machine string, args ...string) error {
	out, err := e.pb(machine, args...)
	if err != nil {
		return err
	}
	if got := lastLines(strings.TrimSpace(out), 1); got != want {
		return fmt.Errorf("pb %s printed %q, want %q", strings.Join(args, " "), lastBytes(out, 300), want)
	}
	return nil
}

// run runs a command from the repository root with a bound; the error carries its output.
func (e *e2eCore) run(bound time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = e.root
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, lastBytes(stdout.String()+stderr.String(), 600))
	}
	return stdout.String(), nil
}

// docker and pb run one command in a machine. Their 45 s bound keeps a hung command from outlasting the
// waitFor around it; one-shot steps that need longer (install, pair) call run with their own bound.
func (e *e2eCore) docker(machine string, args ...string) (string, error) {
	return e.run(45*time.Second, "docker", append([]string{"exec", machine}, args...)...)
}

// pb runs the installed pb in a machine as its user pbt.
func (e *e2eCore) pb(machine string, args ...string) (string, error) {
	base := []string{"exec", "-u", "pbt", "-e", "HOME=/home/pbt", "-e", "XDG_RUNTIME_DIR=/run/user/1000", "-w", "/home/pbt", machine, ".local/bin/pb"}
	return e.run(45*time.Second, "docker", append(base, args...)...)
}

func (e *e2eCore) certificates() error {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "e2e-ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: e.host}, DNSNames: []string{e.host}, IPAddresses: []net.IP{net.ParseIP(e.gateway)},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return err
	}
	for name, block := range map[string]*pem.Block{"ca.pem": {Type: "CERTIFICATE", Bytes: caDER}, "tls.pem": {Type: "CERTIFICATE", Bytes: leafDER}, "tls.key": {Type: "EC PRIVATE KEY", Bytes: keyDER}} {
		if err := os.WriteFile(filepath.Join(e.work, name), pem.EncodeToMemory(block), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (e *e2eCore) buildPB() error {
	p, url := "github.com/pinksaucepasta/paperboat/internal/buildinfo", "https://"+e.host+":"+e.port
	commit, err := e.run(time.Minute, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	flags := fmt.Sprintf("-X %[1]s.Version=dev-e2e -X %[1]s.Commit=%[2]s -X %[1]s.ProtocolVersion=1 -X %[1]s.DefaultServerURL=%[3]s -X %[1]s.DefaultReleaseURL=%[3]s", p, strings.TrimSpace(commit), url)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags", flags, "-o", filepath.Join(e.work, "pb"), "./cmd/pb")
	cmd.Dir, cmd.Env = filepath.Join(e.root, "cli"), append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build pb: %w: %s", err, lastBytes(string(out), 2000))
	}
	return nil
}

// startServer runs this test binary as TestE2EServe. The first start seeds the account and writes the
// enrollment tokens; every start is ready once /healthz answers through the TLS front the machines use.
func (e *e2eCore) startServer(first bool) error {
	log, err := os.OpenFile(filepath.Join(e.work, "server.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(os.Args[0], "-test.run", "^TestE2EServe$", "-test.v", "-test.timeout", "30m")
	cmd.Env = append(os.Environ(), "PAPERBOAT_E2E_DIR="+e.work, "PAPERBOAT_E2E_MINT_KEY="+e.mintKey, "PAPERBOAT_E2E_URL=https://"+e.host+":"+e.port,
		"PAPERBOAT_E2E_LISTEN=0.0.0.0:"+e.port, "PAPERBOAT_E2E_INTERNAL="+e.internal, "PAPERBOAT_E2E_TLS_CERT="+filepath.Join(e.work, "tls.pem"), "PAPERBOAT_E2E_TLS_KEY="+filepath.Join(e.work, "tls.key"),
		"PAPERBOAT_E2E_ACME_URL=https://localhost:14000/dir", "PAPERBOAT_E2E_EDGE_CREDENTIAL="+e.mintKey+"-edge-credential",
		"PAPERBOAT_E2E_CAA_RESOLVER="+e.caaResolver, "SSL_CERT_FILE="+filepath.Join(e.work, "server-roots.pem"))
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM} // stops with this test even when it is killed
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	e.server, e.serverDone = cmd, done
	roots := x509.NewCertPool()
	if pemCA, err := os.ReadFile(filepath.Join(e.work, "ca.pem")); err != nil || !roots.AppendCertsFromPEM(pemCA) {
		return fmt.Errorf("read the test CA: %v", err)
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: e.host},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:"+e.port)
		}}}
	e.serverClient = client
	return waitFor(3*time.Minute, func() error {
		select {
		case err := <-done:
			done <- err
			return backoffStop{fmt.Errorf("server exited: %v", err)}
		default:
		}
		if first {
			if _, err := os.Stat(filepath.Join(e.work, "tokens.json")); err != nil {
				return errors.New("no enrollment tokens yet")
			}
		}
		response, err := client.Get("https://" + e.host + ":" + e.port + "/healthz")
		if err != nil {
			return err
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("healthz: %s", response.Status)
		}
		return nil
	})
}

// backoffStop is an error waitFor returns at once instead of retrying (an exited server).
type backoffStop struct{ error }

// restartServer stops the server as a deploy does (SIGTERM, bounded drain) and starts it again.
func (e *e2eCore) restartServer() error {
	if err := e.stopServer(); err != nil {
		return err
	}
	return e.startServer(false)
}

func (e *e2eCore) stopServer() error {
	if e.server == nil {
		return nil
	}
	_ = e.server.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-e.serverDone:
		e.server = nil
		if err != nil {
			return fmt.Errorf("server did not stop cleanly on SIGTERM: %v", err)
		}
		return nil
	case <-time.After(60 * time.Second):
		_ = e.server.Process.Kill()
		e.server = nil
		return errors.New("server did not stop within 60 s of SIGTERM")
	}
}

// startMachine starts a systemd container, trusts the test CA and copies pb into it.
func (e *e2eCore) startMachine(name string) error {
	_, _ = e.run(time.Minute, "docker", "rm", "-f", name)
	if _, err := e.run(2*time.Minute, "docker", "run", "-d", "--name", name, "--hostname", name, "--privileged", "--cgroupns=host",
		"-v", "/sys/fs/cgroup:/sys/fs/cgroup:rw", "--tmpfs", "/run", "--tmpfs", "/run/lock",
		"--add-host", e.host+":"+e.gateway, "paperboat-e2e-machine"); err != nil {
		return err
	}
	if err := waitFor(90*time.Second, func() error {
		out, _ := e.docker(name, "systemctl", "is-system-running")
		if state := strings.TrimSpace(out); state != "running" && state != "degraded" {
			return fmt.Errorf("systemd is %q", state)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"cp", filepath.Join(e.work, "ca.pem"), name + ":/usr/local/share/ca-certificates/e2e-ca.crt"},
		{"exec", name, "update-ca-certificates"},
		// Own SSH host keys: the image's would be identical on both machines (pb ssh refuses clones).
		{"exec", name, "bash", "-c", "rm -f /etc/ssh/ssh_host_* && ssh-keygen -A && systemctl restart ssh"},
		{"exec", name, "bash", "-c", "loginctl enable-linger pbt && systemctl start user@1000.service"},
		{"cp", filepath.Join(e.work, "pb"), name + ":/home/pbt/pb"},
		{"exec", name, "bash", "-c", "chown pbt:pbt /home/pbt/pb && chmod 755 /home/pbt/pb"},
	} {
		if _, err := e.run(2*time.Minute, "docker", args...); err != nil {
			return err
		}
	}
	return nil
}

// pair installs pb in the machine and pairs it with its enrollment token, as the console's Add machine does.
func (e *e2eCore) pair(name string) error {
	raw, err := os.ReadFile(filepath.Join(e.work, "tokens.json"))
	if err != nil {
		return err
	}
	var tokens map[string]string
	if err := json.Unmarshal(raw, &tokens); err != nil {
		return err
	}
	if out, err := e.run(2*time.Minute, "docker", "exec", "-u", "pbt", "-e", "HOME=/home/pbt", "-e", "XDG_RUNTIME_DIR=/run/user/1000", "-w", "/home/pbt", name, "./pb", "install"); err != nil {
		return fmt.Errorf("pb install: %w (%s)", err, lastBytes(out, 400))
	}
	tokenFile := filepath.Join(e.work, "token-"+name)
	if err := os.WriteFile(tokenFile, []byte(tokens[name]), 0o600); err != nil {
		return err
	}
	if _, err := e.run(time.Minute, "docker", "cp", tokenFile, name+":/home/pbt/token"); err != nil {
		return err
	}
	if _, err := e.docker(name, "bash", "-c", "chown pbt:pbt /home/pbt/token && chmod 600 /home/pbt/token"); err != nil {
		return err
	}
	out, err := e.run(2*time.Minute, "docker", "exec", "-u", "pbt", "-e", "HOME=/home/pbt", "-e", "XDG_RUNTIME_DIR=/run/user/1000", "-w", "/home/pbt",
		name, ".local/bin/pb", "pair", "--enrollment-token-file", "/home/pbt/token", "--name", name, "--json")
	if err != nil {
		return err
	}
	var result struct{ OK bool }
	if json.Unmarshal([]byte(out), &result) != nil || !result.OK {
		return fmt.Errorf("pair answered: %s", lastBytes(out, 400))
	}
	_, err = e.docker(name, "rm", "-f", "/home/pbt/token")
	return err
}

// diagnose prints what the server answered and what each daemon logged, for the failing step.
func (e *e2eCore) diagnose() {
	for _, log := range []string{"server.log", "edge.log"} {
		if raw, err := os.ReadFile(filepath.Join(e.work, log)); err == nil {
			e.t.Logf("--- %s (last lines)\n%s", log, lastLines(string(raw), 80))
		}
	}
	for _, name := range e.machines {
		out, _ := e.run(20*time.Second, "docker", "exec", "-u", "pbt", "-e", "XDG_RUNTIME_DIR=/run/user/1000", name, "journalctl", "--user", "--no-pager", "-n", "60")
		e.t.Logf("--- %s user journal\n%s", name, out)
		out, _ = e.run(20*time.Second, "docker", "exec", name, "journalctl", "--no-pager", "-n", "60", "-u", "paperboat*")
		e.t.Logf("--- %s system journal (paperboat units)\n%s", name, out)
	}
}

func (e *e2eCore) cleanup() {
	_ = os.WriteFile(filepath.Join(e.work, "stop"), nil, 0o600)
	_ = e.stopEdge()
	_ = e.stopServer()
	for _, name := range append([]string{"e2e-pebble"}, e.machines...) {
		_, _ = e.run(time.Minute, "docker", "rm", "-f", name)
	}
}

// freePort is a port in 20000-29999 free on host now; the server binds it moments later. Not the kernel's
// ephemeral range: a firewall that admits containers to the host (anvit: 10000-59999) may not cover it.
func freePort(t *testing.T, host string) string {
	for range 100 {
		port := strconv.Itoa(20000 + mathrand.IntN(10000))
		if listener, err := net.Listen("tcp", net.JoinHostPort(host, port)); err == nil {
			listener.Close()
			return port
		}
	}
	t.Fatal("no free port in 20000-29999")
	return ""
}

func lastBytes(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}
