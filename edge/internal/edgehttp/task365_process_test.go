package edgehttp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTask365ServerIssuedTLSProcesses exercises SQL-issued, expiring authority
// with actual daemon/edge processes. The database and executables are supplied
// by the remote coordinator; no local backend or database is launched.
func TestTask365ServerIssuedTLSProcesses(t *testing.T) {
	daemonBinary, serverBinary := os.Getenv("PAPERBOAT_TASK365_DAEMON_TEST_BINARY"), os.Getenv("PAPERBOAT_TASK365_SERVER_TEST_BINARY")
	if daemonBinary == "" || serverBinary == "" || os.Getenv("PAPERBOAT_TEST_DATABASE_DSN") == "" {
		t.Skip("requires remote isolated database and built server/daemon test executables")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	directory := t.TempDir()
	var endpointBytes [16]byte
	if _, err := rand.Read(endpointBytes[:]); err != nil {
		t.Fatal(err)
	}
	endpointBytes[6] = endpointBytes[6]&0x0f | 0x40
	endpointBytes[8] = endpointBytes[8]&0x3f | 0x80
	host := fmt.Sprintf("%08x-%04x-%04x-%04x-%012x.tunnels.example.test", endpointBytes[0:4], endpointBytes[4:6], endpointBytes[6:8], endpointBytes[8:10], endpointBytes[10:16])
	httpHost := "app-" + strings.SplitN(host, ".", 2)[0] + ".customer.test"
	endpointBytes[15] ^= 1
	secondHost := fmt.Sprintf("%08x-%04x-%04x-%04x-%012x.tunnels.example.test", endpointBytes[0:4], endpointBytes[4:6], endpointBytes[6:8], endpointBytes[8:10], endpointBytes[10:16])
	clientTLS, serverTLS, ca, clientCert, clientKey := task24Certificates(t, host, httpHost)
	secondClientTLS, secondServerTLS, _, _, _ := task24Certificates(t, secondHost)
	secondClientTLS.ServerName = secondHost
	clientTLS.ServerName = host
	origin, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	secondOrigin, err := tls.Listen("tcp", "127.0.0.1:0", secondServerTLS)
	if err != nil {
		_ = origin.Close()
		t.Fatal(err)
	}
	var originWorkers sync.WaitGroup
	for _, origin := range []net.Listener{origin, secondOrigin} {
		originWorkers.Go(func() {
			for {
				conn, err := origin.Accept()
				if err != nil {
					return
				}
				originWorkers.Go(func() {
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
					data, err := io.ReadAll(conn)
					if err == nil {
						_, _ = conn.Write(data)
					}
				})
			}
		})
	}
	defer func() { _ = origin.Close(); _ = secondOrigin.Close(); originWorkers.Wait() }()
	freePort := func(ip string) string {
		t.Helper()
		l, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	}
	httpOriginAddress := net.JoinHostPort("127.0.0.1", freePort("127.0.0.1"))
	tlsPort, httpPort, tcpPort := "443", "443", freePort("127.0.0.2")
	cert := serverTLS.Certificates[0]
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	type child struct {
		cmd  *exec.Cmd
		log  *os.File
		path string
		done chan struct{}
		err  error
	}
	var children []*child
	start := func(binary, name, variable, path string) {
		t.Helper()
		logPath := filepath.Join(directory, fmt.Sprintf("child-%d.log", len(children)))
		log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, binary, "-test.run=^"+name+"$", "-test.count=1", "-test.timeout=100s")
		cmd.Env = append(os.Environ(), variable+"="+path)
		cmd.Stdout = log
		cmd.Stderr = log
		if err = cmd.Start(); err != nil {
			log.Close()
			t.Fatal(err)
		}
		c := &child{cmd: cmd, log: log, path: logPath, done: make(chan struct{})}
		children = append(children, c)
		go func() {
			c.err = c.cmd.Wait()
			close(c.done)
			if c.err != nil {
				cancel()
			}
		}()
	}
	defer func() {
		cancel()
		for _, c := range children {
			_ = c.cmd.Process.Kill()
			<-c.done
			_ = c.log.Close()
			if t.Failed() {
				data, _ := os.ReadFile(c.path)
				t.Logf("fixture output: %s", data)
			}
		}
	}()
	fixtures := make([]task30EdgeFixture, 2)
	edgeAuthority := make([]map[string]string, 2)
	for i := range 2 {
		prefix := filepath.Join(directory, fmt.Sprint(i))
		fixtures[i] = task30EdgeFixture{AdditionalCarrierPath: prefix + "-second-carrier", DynamicAuthority: true, LifetimeSeconds: 90, Node: task30CarrierNode{NodeID: fmt.Sprintf("edge_tls_0%d", i), Epoch: fmt.Sprintf("epoch_tls_000%d", i), Domain: fmt.Sprintf("tls_region_%d", i)}, IP: fmt.Sprintf("127.0.0.%d", i+2), HTTPPort: httpPort, TCPPort: tcpPort, CA: string(ca), Cert: string(certPEM), Key: string(keyPEM), CarrierPath: prefix + "-carrier", AuthorityPath: prefix + "-authority", ReadyPath: prefix + "-ready", PublicTLSListenHost: fmt.Sprintf("127.0.0.%d", i+2), CarrierListenAddress: fmt.Sprintf("127.0.0.%d:0", i+2)}
		edgeAuthority[i] = map[string]string{"node_id": fixtures[i].Node.NodeID, "epoch": fixtures[i].Node.Epoch, "path": fixtures[i].AuthorityPath}
	}
	authorityConfig := filepath.Join(directory, "sql-config")
	port, _ := strconv.Atoi(tlsPort)
	markers := map[string]string{"partition_path": filepath.Join(directory, "partition"), "stop_path": filepath.Join(directory, "sql-stop"), "ready_path": filepath.Join(directory, "sql-ready"), "remove_tls_path": filepath.Join(directory, "remove"), "disable_edge_path": filepath.Join(directory, "disable"), "restore_edge_path": filepath.Join(directory, "restore"), "revoke_pair_path": filepath.Join(directory, "revoke-pair"), "restore_pair_path": filepath.Join(directory, "restore-pair"), "selfhost_only_path": filepath.Join(directory, "selfhost-only")}
	config := map[string]any{"edges": edgeAuthority, "hostname": httpHost, "origin_address": httpOriginAddress, "tls_port": port, "tls_routes": []map[string]string{{"route_id": "route_tls", "hostname": host, "origin_address": origin.Addr().String()}, {"route_id": "route_tls_second", "hostname": secondHost, "origin_address": secondOrigin.Addr().String()}}, "selfhost": true}
	for key, value := range markers {
		config[key] = value
	}
	task30Write(t, authorityConfig, config)
	start(serverBinary, "TestTask35LiveIngressAuthority", "PAPERBOAT_TASK35_AUTHORITY_CONFIG", authorityConfig)
	var ready struct {
		EdgeNodeIDs []string `json:"edge_node_ids"`
		TLSRouteID  string   `json:"tls_route_id"`
		TLSRouteIDs []string `json:"tls_route_ids"`
	}
	task30Read(t, ctx, markers["ready_path"], &ready)
	if len(ready.EdgeNodeIDs) != len(fixtures) || len(ready.TLSRouteIDs) != 2 {
		t.Fatal("authority fixture omitted edge nodes")
	}
	for i := range fixtures {
		fixtures[i].Node.NodeID = ready.EdgeNodeIDs[i]
	}
	var authorities task24Authority
	waitFor := func(check func() bool) {
		t.Helper()
		until := time.Now().Add(15 * time.Second)
		for !check() {
			if ctx.Err() != nil || time.Now().After(until) {
				t.Fatal("TLS process checkpoint timed out")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor(func() bool {
		data, err := os.ReadFile(fixtures[0].AuthorityPath)
		return err == nil && json.Unmarshal(data, &authorities) == nil && len(authorities.TLS) == 2
	})
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	nodes := make([]task30CarrierNode, 2)
	secondNodes := make([]task30CarrierNode, 2)
	for i, f := range fixtures {
		path := filepath.Join(directory, fmt.Sprintf("edge-%d", i))
		task30Write(t, path, f)
		start(self, "TestTask30EdgeProcess", "PAPERBOAT_TASK35C_EDGE", path)
		task30Read(t, ctx, f.CarrierPath, &nodes[i])
		task30Read(t, ctx, f.AdditionalCarrierPath, &secondNodes[i])
	}
	daemonAuthority := filepath.Join(directory, "daemon-authority")
	secondDaemonAuthority := filepath.Join(directory, "second-daemon-authority")
	merge := func() error {
		var merged task24Authority
		for i, f := range fixtures {
			data, err := os.ReadFile(f.AuthorityPath)
			if err != nil {
				return err
			}
			var a task24Authority
			if err = json.Unmarshal(data, &a); err != nil {
				return err
			}
			if i == 0 {
				merged = a
			}
			merged.Nodes = append(merged.Nodes, a)
		}
		data, err := json.Marshal(merged)
		if err != nil {
			return err
		}
		if err = os.WriteFile(daemonAuthority+".incoming", data, 0600); err != nil {
			return err
		}
		if err := os.Rename(daemonAuthority+".incoming", daemonAuthority); err != nil {
			return err
		}
		filter := func(a task24Authority) task24Authority {
			out := task24Authority{}
			for _, d := range a.TLS {
				if d.Binding.RouteID == ready.TLSRouteIDs[1] {
					out.TLS = append(out.TLS, d)
				}
			}
			return out
		}
		second := filter(merged)
		for _, node := range merged.Nodes {
			second.Nodes = append(second.Nodes, filter(node))
		}
		data, err = json.Marshal(second)
		if err != nil {
			return err
		}
		if err := os.WriteFile(secondDaemonAuthority+".incoming", data, 0600); err != nil {
			return err
		}
		return os.Rename(secondDaemonAuthority+".incoming", secondDaemonAuthority)
	}
	if err = merge(); err != nil {
		t.Fatal(err)
	}
	mergeDone := make(chan struct{})
	go func() {
		defer close(mergeDone)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = merge()
			}
		}
	}()
	defer func() { cancel(); <-mergeDone }()
	descriptor := map[string]any{"Protocol": "http2", "CACert": string(ca), "ClientCert": string(clientCert), "ClientKey": string(clientKey), "OriginsPath": filepath.Join(directory, "origins"), "AuthorityPath": daemonAuthority, "ReadyPath": filepath.Join(directory, "daemon-ready"), "StopPath": filepath.Join(directory, "daemon-stop"), "Nodes": nodes, "DynamicAuthority": true, "FixtureTimeoutSeconds": 90, "HTTPOriginListenAddress": httpOriginAddress, "TLSOrigins": []map[string]string{{"RouteID": ready.TLSRouteIDs[0], "Hostname": host, "Address": origin.Addr().String()}}}
	descriptorPath := filepath.Join(directory, "daemon")
	task30Write(t, descriptorPath, descriptor)
	start(daemonBinary, "TestTask24CrossRepositoryDaemon", "PAPERBOAT_TASK24_DESCRIPTOR", descriptorPath)
	secondDescriptor := map[string]any{"Protocol": "http2", "CACert": string(ca), "ClientCert": string(clientCert), "ClientKey": string(clientKey), "OriginsPath": filepath.Join(directory, "second-origins"), "AuthorityPath": secondDaemonAuthority, "ReadyPath": filepath.Join(directory, "second-daemon-ready"), "StopPath": filepath.Join(directory, "second-daemon-stop"), "Nodes": secondNodes, "DynamicAuthority": true, "TLSOnly": true, "FixtureTimeoutSeconds": 90, "TLSOrigins": []map[string]string{{"RouteID": ready.TLSRouteIDs[1], "Hostname": secondHost, "Address": secondOrigin.Addr().String()}}}
	secondDescriptorPath := filepath.Join(directory, "second-daemon")
	task30Write(t, secondDescriptorPath, secondDescriptor)
	start(daemonBinary, "TestTask24CrossRepositoryDaemon", "PAPERBOAT_TASK24_DESCRIPTOR", secondDescriptorPath)
	var processReady any
	for _, f := range fixtures {
		task30Read(t, ctx, f.ReadyPath, &processReady)
	}
	waitFor(func() bool { _, err := os.Stat(descriptor["ReadyPath"].(string)); return err == nil })
	waitFor(func() bool { _, err := os.Stat(secondDescriptor["ReadyPath"].(string)); return err == nil })
	dial := func(i int) (*tls.Conn, error) {
		return tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", net.JoinHostPort(fixtures[i].IP, tlsPort), clientTLS)
	}
	roundtripOrigin := func(i int, clientConfig *tls.Config, expected tls.Certificate) {
		t.Helper()
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", net.JoinHostPort(fixtures[i].IP, tlsPort), clientConfig)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if !bytes.Equal(conn.ConnectionState().PeerCertificates[0].Raw, expected.Certificate[0]) {
			t.Fatal("origin certificate changed")
		}
		payload := bytes.Repeat([]byte("TLS-through-SQL-authority\x00"), 4096)
		if _, err = conn.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err = conn.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(conn)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("TLS roundtrip: %v", err)
		}
	}
	roundtrip := func(i int) {
		roundtripOrigin(i, clientTLS, cert)
		roundtripOrigin(i, secondClientTLS, secondServerTLS.Certificates[0])
	}
	checkHTTPS := func(i int) {
		t.Helper()
		config := clientTLS.Clone()
		config.ServerName = httpHost
		transport := &http.Transport{TLSClientConfig: config, ForceAttemptHTTP2: true, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != net.JoinHostPort(httpHost, "443") {
				return nil, fmt.Errorf("unexpected HTTPS address %s", address)
			}
			return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, network, net.JoinHostPort(fixtures[i].IP, "443"))
		}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
		response, err := client.Post("https://"+httpHost+"/stream", "text/plain", strings.NewReader("task24-upload"))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != 200 || response.ProtoMajor != 2 || string(body) != "task24-origin-ok" {
			t.Fatalf("shared443 HTTPS response status=%d protocol=%s body=%q error=%v", response.StatusCode, response.Proto, body, err)
		}
	}
	checkWebSocket := func(i int) {
		t.Helper()
		config := clientTLS.Clone()
		config.ServerName = httpHost
		config.NextProtos = []string{"http/1.1"}
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", net.JoinHostPort(fixtures[i].IP, "443"), config)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := fmt.Fprintf(conn, "GET /socket HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: MDEyMzQ1Njc4OWFiY2RlZg==\r\n\r\n", httpHost); err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(conn)
		response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusSwitchingProtocols {
			response.Body.Close()
			t.Fatalf("WebSocket upgrade status %d", response.StatusCode)
		}
		early := make([]byte, 5)
		if _, err := io.ReadFull(reader, early); err != nil || string(early) != "early" {
			t.Fatalf("WebSocket early bytes: %q %v", early, err)
		}
		if _, err := conn.Write([]byte("late")); err != nil {
			t.Fatal(err)
		}
		echo := make([]byte, 9)
		if _, err := io.ReadFull(reader, echo); err != nil || string(echo) != "echo:late" {
			t.Fatalf("WebSocket bidirectional bytes: %q %v", echo, err)
		}
	}
	for i := range fixtures {
		checkHTTPS(i)
		roundtrip(i)
		checkWebSocket(i)
		checkHTTPS(i)
	}
	task30Write(t, markers["selfhost_only_path"], true)
	authorityPresent := func(i int, present bool) bool {
		data, err := os.ReadFile(fixtures[i].AuthorityPath)
		var current task24Authority
		return err == nil && json.Unmarshal(data, &current) == nil && (len(current.TLS) > 0) == present
	}
	waitFor(func() bool { return authorityPresent(0, true) })
	waitFor(func() bool { return authorityPresent(1, false) })
	if conn, dialErr := dial(1); dialErr == nil {
		conn.Close()
		t.Fatal("managed edge accepted self-hosted-only route")
	}
	task30Write(t, markers["revoke_pair_path"], true)
	waitFor(func() bool { return authorityPresent(0, false) })
	waitFor(func() bool {
		conn, err := dial(0)
		if err != nil {
			return true
		}
		conn.Close()
		return false
	})
	if conn, dialErr := dial(1); dialErr == nil {
		conn.Close()
		t.Fatal("managed fallback appeared during self-hosted outage")
	}
	task30Write(t, markers["restore_pair_path"], true)
	waitFor(func() bool { return authorityPresent(0, true) })
	waitFor(func() bool {
		conn, err := dial(0)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	})
	roundtrip(0)
	task30Write(t, markers["remove_tls_path"], true)
	for i := range 2 {
		waitFor(func() bool { return authorityPresent(i, false) })
	}
	task30Write(t, markers["stop_path"], true)
	task30Write(t, descriptor["StopPath"].(string), true)
	task30Write(t, secondDescriptor["StopPath"].(string), true)
	for _, c := range []*child{children[0], children[len(children)-2], children[len(children)-1]} {
		select {
		case <-c.done:
			if c.err != nil {
				t.Fatalf("fixture cleanup failed: %v", c.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("fixture cleanup did not complete")
		}
	}
	t.Log("shared TCP443 HTTPS HTTP/2, WebSocket upgrade bytes and two distinct TLS origins, self-hosted-only TLS forwarding, managed isolation, pairing revocation and re-pair recovery passed")
	return

}
