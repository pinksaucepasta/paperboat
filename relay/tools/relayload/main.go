// Command relayload is the load generator of the relay capacity runs: it connects peers to a real paperboat-relay with signed grants
// (the issuer key is the one loadctl wrote) and sends paced WireGuard-sized packets between them.
//
//	relayload idle -dir D -relay HOST:PORT -n N        connect N peers, hold them, report how many were admitted
//	relayload flow -dir D -relay HOST:PORT -pairs P -mbit 10 -duration 30s
//
// Each pair is one relayed stream (a sender and a receiver of one account that share a resource scope). A development tool; never installed.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tailscale.com/derp"
	"tailscale.com/types/key"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

type env struct {
	dir, relay, sni, issuer, nodeID string
	generation                      uint64
	epoch                           string
	signing                         ed25519.PrivateKey
	roots                           *x509.CertPool
	accountSize                     int
}

func load(f *flag.FlagSet, args []string) (*env, error) {
	e := &env{}
	f.StringVar(&e.dir, "dir", "", "loadctl directory (grant.seed, relay.crt, credential)")
	f.StringVar(&e.relay, "relay", "127.0.0.1:27500", "relay UDP address")
	f.StringVar(&e.sni, "sni", "relay.example.test", "relay certificate name")
	f.StringVar(&e.issuer, "issuer", "loadctl", "issuer the relay was started with")
	f.StringVar(&e.nodeID, "node", "relay_load", "relay node id")
	f.IntVar(&e.accountSize, "account-size", 200, "peers per account (idle)")
	var generation = f.Uint64("generation", 0, "relay node generation (default: asked from loadctl)")
	var epoch = f.String("epoch", "", "relay process epoch (default: asked from loadctl)")
	var control = f.String("control", "https://127.0.0.1:8443", "loadctl URL, for the relay's generation and epoch")
	if err := f.Parse(args); err != nil {
		return nil, err
	}
	seed, err := os.ReadFile(filepath.Join(e.dir, "grant.seed"))
	if err != nil {
		return nil, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(seed)))
	if err != nil {
		return nil, err
	}
	e.signing = ed25519.NewKeyFromSeed(raw)
	certPEM, err := os.ReadFile(filepath.Join(e.dir, "relay.crt"))
	if err != nil {
		return nil, err
	}
	e.roots = x509.NewCertPool()
	e.roots.AppendCertsFromPEM(certPEM)
	e.generation, e.epoch = *generation, *epoch
	if e.generation == 0 || e.epoch == "" {
		node, err := askControl(e.dir, *control)
		if err != nil {
			return nil, fmt.Errorf("the relay has not started against loadctl yet: %w", err)
		}
		e.generation, e.epoch = node.Generation, node.Epoch
	}
	return e, nil
}

func certificate() (tls.Certificate, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "peer"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(12 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(raw)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{raw}, PrivateKey: priv, Leaf: leaf}, nil
}

// peer is one connected relay client.
type peer struct {
	client *derpquic.Client
	key    key.NodePublic
}

func (e *env) newPeer(account, endpoint string, scope *peernetwork.Scope) (*peer, error) {
	cert, err := certificate()
	if err != nil {
		return nil, err
	}
	node := key.NewNode().Public()
	fingerprint := sha256.Sum256(cert.Certificate[0])
	now := time.Now().Unix()
	grant := peernetwork.RelayGrant{Version: 1, Issuer: e.issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: now + 3600, Generation: 1, AccountID: account, EndpointID: endpoint,
		WireGuardPublicKey: derpquic.KeyString(node), CertificateFingerprint: hex.EncodeToString(fingerprint[:]), QUICPublicKey: base64.RawURLEncoding.EncodeToString(cert.PrivateKey.(ed25519.PrivateKey).Public().(ed25519.PublicKey)),
		NodeID: e.nodeID, NodeGeneration: e.generation, ProcessEpoch: e.epoch}
	if scope != nil {
		grant.Memberships = []peernetwork.Scope{*scope}
	}
	body, err := json.Marshal(grant)
	if err != nil {
		return nil, err
	}
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"paperboat-relay-grant+jwt","kid":"loadctl"}`))
	signed := head + "." + base64.RawURLEncoding.EncodeToString(body)
	token := signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(e.signing, []byte(signed)))
	client := derpquic.NewClient(derpquic.ClientConfig{Address: e.relay, TLS: &tls.Config{RootCAs: e.roots, ServerName: e.sni, Certificates: []tls.Certificate{cert}},
		Credential: func(context.Context) (string, error) { return token, nil }})
	return &peer{client: client, key: node}, nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: relayload idle|flow [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "idle":
		err = idle(os.Args[2:])
	case "flow":
		err = flow(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "relayload:", err)
		os.Exit(1)
	}
}

func idle(args []string) error {
	f := flag.NewFlagSet("idle", flag.ExitOnError)
	n := f.Int("n", 1000, "peers to connect")
	start := f.Int("start", 0, "first peer index (shard a run)")
	rate := f.Float64("rate", 300, "connections started per second")
	hold := f.Duration("hold", 0, "hold time after the ramp (0 = until interrupted)")
	e, err := load(f, args)
	if err != nil {
		return err
	}
	var ok, failed atomic.Int64
	var mu sync.Mutex
	var latencies []time.Duration
	reasons := map[string]int{}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 128)
	var peers []*peer
	begin := time.Now()
	for i := *start; i < *start+*n; i++ {
		time.Sleep(time.Duration(float64(time.Second) / *rate))
		p, err := e.newPeer(fmt.Sprintf("account_%d", i/e.accountSize), fmt.Sprintf("endpoint_%d", i), nil)
		if err != nil {
			return err
		}
		peers = append(peers, p)
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			began := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := p.client.Connect(ctx); err != nil {
				failed.Add(1)
				text := err.Error()
				if len(text) > 120 {
					text = text[:120]
				}
				mu.Lock()
				reasons[text]++
				mu.Unlock()
				return
			}
			ok.Add(1)
			mu.Lock()
			latencies = append(latencies, time.Since(began))
			mu.Unlock()
		}()
	}
	wg.Wait()
	mu.Lock()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	pct := func(q float64) int64 {
		if len(latencies) == 0 {
			return 0
		}
		return latencies[int(float64(len(latencies)-1)*q)].Milliseconds()
	}
	out := map[string]any{"mode": "idle", "connected": ok.Load(), "failed": failed.Load(), "ramp_seconds": time.Since(begin).Seconds(), "connect_ms_p50": pct(0.5), "connect_ms_p99": pct(0.99), "errors": reasons}
	mu.Unlock()
	data, _ := json.Marshal(out)
	fmt.Println(string(data))
	if *hold > 0 {
		time.Sleep(*hold)
	} else {
		select {}
	}
	for _, p := range peers {
		_ = p.client.Close()
	}
	return nil
}

// flow runs P pairs, each in its own account, each sender pacing -mbit Mbit/s of 1312-byte packets to its receiver.
func flow(args []string) error {
	f := flag.NewFlagSet("flow", flag.ExitOnError)
	pairs := f.Int("pairs", 10, "relayed streams")
	mbit := f.Float64("mbit", 10, "offered rate per stream")
	size := f.Int("size", 1312, "packet bytes")
	duration := f.Duration("duration", 30*time.Second, "send time")
	e, err := load(f, args)
	if err != nil {
		return err
	}
	type streamStats struct{ sent, received, bytes atomic.Int64 }
	stats := make([]*streamStats, *pairs)
	var latMu sync.Mutex
	var latencies []time.Duration
	var wg sync.WaitGroup
	senders := make([]*peer, *pairs)
	receivers := make([]*peer, *pairs)
	targets := make([]key.NodePublic, *pairs)
	connectFailed := 0
	// A fresh name per run: the relay fences an endpoint's grant generation, so a second run with the same names and new keys is refused.
	run := fmt.Sprintf("%x", time.Now().UnixNano())
	for i := 0; i < *pairs; i++ {
		stats[i] = &streamStats{}
		account := fmt.Sprintf("account_f%s_%d", run, i)
		scope := peernetwork.Scope{ResourceKind: "machine_access", ResourceID: fmt.Sprintf("machine_f%d", i), ResourceGeneration: 1, Capability: "terminal", Direction: "dial", Port: 443, ExpiresAt: time.Now().Unix() + 3600}
		accept := scope
		accept.Direction = "accept"
		s, err := e.newPeer(account, fmt.Sprintf("send_%s_%d", run, i), &scope)
		if err != nil {
			return err
		}
		r, err := e.newPeer(account, fmt.Sprintf("recv_%s_%d", run, i), &accept)
		if err != nil {
			return err
		}
		senders[i], receivers[i], targets[i] = s, r, r.key
	}
	sem := make(chan struct{}, 64)
	var cwg sync.WaitGroup
	var cfail atomic.Int64
	for i := 0; i < *pairs; i++ {
		for _, p := range []*peer{senders[i], receivers[i]} {
			sem <- struct{}{}
			cwg.Add(1)
			go func() {
				defer cwg.Done()
				defer func() { <-sem }()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := p.client.Connect(ctx); err != nil {
					if cfail.Add(1) == 1 {
						fmt.Fprintln(os.Stderr, "first connect error:", err)
					}
				}
			}()
		}
	}
	cwg.Wait()
	connectFailed = int(cfail.Load())
	stop := make(chan struct{})
	for i := 0; i < *pairs; i++ {
		i := i
		wg.Add(2)
		go func() { // receiver
			defer wg.Done()
			for {
				m, _, err := receivers[i].client.RecvDetail()
				if err != nil {
					return
				}
				if p, ok := m.(derp.ReceivedPacket); ok && len(p.Data) >= 16 {
					stats[i].received.Add(1)
					stats[i].bytes.Add(int64(len(p.Data)))
					sentAt := int64(binary.BigEndian.Uint64(p.Data[8:16]))
					if i%8 == 0 { // sample the latency of every eighth stream
						latMu.Lock()
						latencies = append(latencies, time.Duration(time.Now().UnixNano()-sentAt))
						latMu.Unlock()
					}
				}
			}
		}()
		go func() { // sender, paced in 5 ms bursts
			defer wg.Done()
			packet := make([]byte, *size)
			packet[0] = 4
			perTick := *mbit * 1e6 / 8 / float64(*size) * 0.005
			var owed float64
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
				}
				owed += perTick
				for owed >= 1 {
					binary.BigEndian.PutUint64(packet[8:16], uint64(time.Now().UnixNano()))
					if senders[i].client.Send(targets[i], packet) == nil {
						stats[i].sent.Add(1)
					}
					owed--
				}
			}
		}()
	}
	began := time.Now()
	time.Sleep(*duration)
	close(stop)
	elapsed := time.Since(began).Seconds()
	time.Sleep(time.Second)
	var sent, received, bytes int64
	for _, s := range stats {
		sent += s.sent.Load()
		received += s.received.Load()
		bytes += s.bytes.Load()
	}
	latMu.Lock()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	pct := func(q float64) float64 {
		if len(latencies) == 0 {
			return -1
		}
		return float64(latencies[int(float64(len(latencies)-1)*q)].Microseconds()) / 1000
	}
	out := map[string]any{"mode": "flow", "pairs": *pairs, "offered_mbit_each": *mbit, "connect_failed": connectFailed, "seconds": elapsed,
		"offered_mbit_total": float64(sent) * float64(*size) * 8 / 1e6 / elapsed, "delivered_mbit_total": float64(bytes) * 8 / 1e6 / elapsed,
		"loss_pct": 100 * float64(sent-received) / float64(max(sent, 1)), "latency_ms_p50": pct(0.5), "latency_ms_p99": pct(0.99)}
	latMu.Unlock()
	data, _ := json.Marshal(out)
	fmt.Println(string(data))
	for i := range senders {
		_ = senders[i].client.Close()
		_ = receivers[i].client.Close()
	}
	return nil
}

type relayNode struct {
	Generation uint64 `json:"generation"`
	Epoch      string `json:"epoch"`
}

// askControl reads the relay node's generation and epoch that loadctl recorded when the relay started.
func askControl(dir, base string) (relayNode, error) {
	var node relayNode
	credential, err := os.ReadFile(filepath.Join(dir, "credential"))
	if err != nil {
		return node, err
	}
	certPEM, err := os.ReadFile(filepath.Join(dir, "control.crt"))
	if err != nil {
		return node, err
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}}}
	request, err := http.NewRequest("GET", base+"/admin/relay", nil)
	if err != nil {
		return node, err
	}
	request.Header.Set("Authorization", "Bearer "+string(credential))
	response, err := client.Do(request)
	if err != nil {
		return node, err
	}
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(&node); err != nil || node.Epoch == "" {
		return node, fmt.Errorf("no relay recorded (%v)", err)
	}
	return node, nil
}
