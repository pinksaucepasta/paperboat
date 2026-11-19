package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/preview"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/tunnelmanager"
	"github.com/pinksaucepasta/paperboat/cli/internal/inspector"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// registration is the part of the edge's node registration (kept by loadctl) that machines need.
type registration struct {
	NodeID          string `json:"edge_node_id"`
	ProcessEpoch    string `json:"process_epoch"`
	CarrierEndpoint struct {
		Host     string `json:"host"`
		TCPPort  uint16 `json:"tcp_port"`
		QUICPort uint16 `json:"quic_port"`
	} `json:"carrier_endpoint"`
	SPKI  string `json:"carrier_server_spki_sha256"`
	Chain string `json:"carrier_server_certificate_chain_pem"`
}

type control struct {
	base       string
	credential string
	client     *http.Client
}

func newControl(dir, base string) (*control, error) {
	credential, err := os.ReadFile(filepath.Join(dir, "credential"))
	if err != nil {
		return nil, err
	}
	certificate, err := os.ReadFile(filepath.Join(dir, "control.crt"))
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certificate)
	return &control{base: base, credential: string(credential), client: &http.Client{Timeout: 60 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}, MaxIdleConnsPerHost: 4}}}, nil
}

func (c *control) call(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.credential)
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("%s %s: %s %s", method, path, response.Status, message)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	return json.NewDecoder(response.Body).Decode(out)
}

// stats are the run's counters; Print writes one JSON line.
type stats struct {
	connected, failed, inflight atomic.Int64
	mu                          sync.Mutex
	latencies                   []time.Duration
	errors                      map[string]int
}

func (s *stats) fail(err error) {
	s.failed.Add(1)
	text := err.Error()
	if len(text) > 160 {
		text = text[:160]
	}
	s.mu.Lock()
	if s.errors == nil {
		s.errors = map[string]int{}
	}
	if len(s.errors) < 20 || s.errors[text] > 0 {
		s.errors[text]++
	}
	s.mu.Unlock()
}

func (s *stats) line(extra map[string]any) []byte {
	s.mu.Lock()
	latencies := append([]time.Duration(nil), s.latencies...)
	failures := map[string]int{}
	for k, v := range s.errors {
		failures[k] = v
	}
	s.mu.Unlock()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	percentile := func(p float64) int64 {
		if len(latencies) == 0 {
			return 0
		}
		return latencies[int(float64(len(latencies)-1)*p)].Milliseconds()
	}
	out := map[string]any{"at": time.Now().UTC().Format(time.RFC3339), "connected": s.connected.Load(), "failed": s.failed.Load(), "inflight": s.inflight.Load(),
		"connect_ms_p50": percentile(0.5), "connect_ms_p99": percentile(0.99), "connect_ms_max": percentile(1), "errors": failures, "rss_mb": rssMB()}
	for k, v := range extra {
		out[k] = v
	}
	data, _ := json.Marshal(out)
	return data
}

func rssMB() int64 {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, _ := strconv.ParseInt(fields[1], 10, 64)
	return pages * int64(os.Getpagesize()) >> 20
}

type machineFlags struct {
	dir, controlURL, stateRoot, domain, runtimeDomain, origin, transport, runID string
	count, routes, start, accountSize, batch, concurrency                       int
	rate                                                                        float64
	hold, statsEvery                                                            time.Duration
	inspector                                                                   bool
}

func machines(args []string) error {
	var f machineFlags
	flags := flag.NewFlagSet("machines", flag.ExitOnError)
	flags.StringVar(&f.dir, "dir", "", "loadctl directory (credential, control.crt)")
	flags.StringVar(&f.controlURL, "control", "https://127.0.0.1:8443", "loadctl URL")
	flags.StringVar(&f.stateRoot, "state-root", "/tmp/loadsim-machines", "machine identity directories")
	flags.StringVar(&f.domain, "domain", "preview.example.test", "endpoint route base domain")
	flags.StringVar(&f.runtimeDomain, "runtime-domain", "runtime.example.test", "runtime route base domain")
	flags.StringVar(&f.origin, "origin", "127.0.0.1:3000", "address of the shared origin every route forwards to")
	flags.StringVar(&f.transport, "transport", "h2", "carrier transport the machines get: h2 or auto (QUIC first, as production)")
	flags.StringVar(&f.runID, "run", "a", "id prefix, so two runs never reuse a machine or route name")
	flags.IntVar(&f.count, "machines", 100, "machines to connect")
	flags.IntVar(&f.start, "start", 0, "first machine index (shard a run over several processes)")
	flags.IntVar(&f.routes, "routes", 1, "endpoint routes (previews or tunnels) per machine")
	flags.IntVar(&f.accountSize, "account-size", 100, "machines per account")
	flags.IntVar(&f.batch, "batch", 100, "machines admitted per control request")
	flags.IntVar(&f.concurrency, "concurrency", 64, "carriers dialed at once")
	flags.Float64Var(&f.rate, "rate", 200, "machines started per second (0 = no limit)")
	flags.BoolVar(&f.inspector, "inspector", false, "capture every request and response of every route in the machines' inspector store (request and body recording on)")
	flags.DurationVar(&f.hold, "hold", 0, "how long to hold the carriers after the ramp (0 = until interrupted)")
	flags.DurationVar(&f.statsEvery, "stats", 10*time.Second, "interval of the JSON stats lines")
	if err := flags.Parse(args); err != nil {
		return err
	}
	ctrl, err := newControl(f.dir, f.controlURL)
	if err != nil {
		return err
	}
	var edge registration
	if err := ctrl.call("GET", "/admin/registration", nil, &edge); err != nil || edge.NodeID == "" {
		return fmt.Errorf("the edge has not registered with loadctl yet: %v", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go serveOrigin(f.origin)
	runner := &runner{f: f, edge: edge, ctrl: ctrl, ctx: ctx, transport: &tunnelmanager.OriginHTTPTransport{}}
	go func() {
		for range time.Tick(f.statsEvery) {
			fmt.Println(string(runner.stats.line(map[string]any{"phase": runner.phase.Load()})))
		}
	}()
	started := time.Now()
	runner.ramp()
	fmt.Println(string(runner.stats.line(map[string]any{"phase": "ramp_done", "ramp_seconds": time.Since(started).Seconds()})))
	if f.hold > 0 {
		select {
		case <-time.After(f.hold):
		case <-ctx.Done():
		}
	} else {
		<-ctx.Done()
	}
	runner.release()
	return nil
}

type runner struct {
	f         machineFlags
	edge      registration
	ctrl      *control
	ctx       context.Context
	transport *tunnelmanager.OriginHTTPTransport
	stats     stats
	phase     atomic.Value
	mu        sync.Mutex
	releases  []func(context.Context) error
}

func (r *runner) release() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, release := range r.releases {
		_ = release(ctx)
	}
}

// machine is one simulated enrolled machine: its identity store, the admissions the edge pulls and the routes it serves.
type machine struct {
	index    int
	hostID   string
	public   string
	runtime  runtimewire.Admission
	routes   []runtimewire.Admission
	source   *preview.MachineAttachmentSessionSource
	admitted preview.RuntimeCarrierAdmission
}

func (r *runner) build(index int) (*machine, error) {
	f := r.f
	name := fmt.Sprintf("%s%d", f.runID, index)
	root := filepath.Join(f.stateRoot, name)
	store, err := identity.Open(identity.Config{StateRoot: root})
	if err != nil {
		return nil, err
	}
	key := store.Current()
	public := base64.RawURLEncoding.EncodeToString(key.Public())
	hostID := "host_load_" + name
	if err := store.SaveRegistration(identity.Registration{ServerURL: "https://control.example.test", MachineID: hostID, EnvironmentID: "environment_load", PublicKeyID: key.ID,
		PublicIdentityKey: public, InboxPath: filepath.Join(root, "inbox"), InstallationGeneration: 1, UpdatedAt: time.Now().UTC()}); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(key.Public())
	thumb := "sha256:" + base64.RawURLEncoding.EncodeToString(digest[:])
	binding := runtimewire.Binding{AccountID: fmt.Sprintf("account_load_%s%d", f.runID, index/f.accountSize), HostID: hostID, TunnelID: "tunnel_load_" + name,
		ConnectorID: "connector_load_" + name, SessionID: "session_load_" + name, ProcessGeneration: 1, ConfigGeneration: 1, InstallationGeneration: 1,
		RouteID: "route_rt_" + name, RouteGeneration: 1, EdgeNodeID: r.edge.NodeID, EdgeProcessEpoch: r.edge.ProcessEpoch,
		EdgeCarrierServerSPKISHA256: r.edge.SPKI, EdgeCarrierServerCertificateChainPEM: r.edge.Chain, MachineIdentityPublicKey: public, MachineIdentityThumbprint: thumb}
	contentHash := "sha256:" + hex.EncodeToString(digest[:])
	endpoints := []string{"h2://" + net.JoinHostPort(r.edge.CarrierEndpoint.Host, strconv.Itoa(int(r.edge.CarrierEndpoint.TCPPort))),
		"h3://" + net.JoinHostPort(r.edge.CarrierEndpoint.Host, strconv.Itoa(int(r.edge.CarrierEndpoint.QUICPort)))}
	expires := time.Now().UTC().Add(time.Hour)
	m := &machine{index: index, hostID: hostID, public: public}
	m.runtime = runtimewire.Admission{Schema: runtimewire.Schema, Binding: binding, AttachmentGeneration: 1, ConfigContentHash: contentHash, EdgeEndpoints: endpoints,
		Hostname: fmt.Sprintf("m%s.%s", name, f.runtimeDomain), RouteKind: runtimewire.RouteKind, RouteRevision: 1, ExpiresAt: expires}
	for route := 0; route < f.routes; route++ {
		global := index*f.routes + route
		a := m.runtime
		a.Binding.RouteID = fmt.Sprintf("route_ep_%s%d", f.runID, global)
		a.Hostname, a.EndpointID, a.AccessMode, a.RouteKind = routeHost(f, global), fmt.Sprintf("endpoint_load_%s%d", f.runID, global), "public", runtimewire.EndpointPublicRouteKind
		m.routes = append(m.routes, a)
	}
	carrier := connector.DefaultDataCarrierPoolConfig()
	carrier.MaximumCarriers = 1
	m.source, err = preview.NewMachineAttachmentSessionSource(preview.MachineAttachmentSessionSourceConfig{StateRoot: root, Carrier: carrier})
	if err != nil {
		return nil, err
	}
	machineEndpoints := endpoints
	if f.transport == "h2" {
		machineEndpoints = endpoints[:1]
	}
	m.admitted = preview.RuntimeCarrierAdmission{AccountID: binding.AccountID, MachineID: hostID, MachineIdentityPublicKey: public, MachineIdentityThumbprint: thumb,
		TunnelID: binding.TunnelID, ConnectorID: binding.ConnectorID, SessionID: binding.SessionID, ProcessGeneration: 1, ConfigGeneration: 1,
		EdgeNodeID: r.edge.NodeID, EdgeProcessEpoch: r.edge.ProcessEpoch, EdgeCarrierServerSPKISHA256: r.edge.SPKI, EdgeCarrierServerCertificateChainPEM: r.edge.Chain,
		EdgeEndpoints: machineEndpoints, ExpiresAt: expires}
	return m, nil
}

func routeHost(f machineFlags, global int) string {
	return fmt.Sprintf("r%s%d.%s", f.runID, global, f.domain)
}

func (r *runner) ramp() {
	r.phase.Store("ramp")
	f := r.f
	var interval time.Duration
	if f.rate > 0 {
		interval = time.Duration(float64(time.Second) / f.rate)
	}
	semaphore := make(chan struct{}, f.concurrency)
	var wg sync.WaitGroup
	var batch []*machine
	flush := func() {
		if len(batch) == 0 {
			return
		}
		body := struct {
			Runtime   []runtimewire.Admission `json:"runtime"`
			Endpoints []runtimewire.Admission `json:"endpoints"`
		}{}
		for _, m := range batch {
			body.Runtime = append(body.Runtime, m.runtime)
			body.Endpoints = append(body.Endpoints, m.routes...)
		}
		if err := r.ctrl.call("POST", "/admin/admit", body, nil); err != nil {
			for range batch {
				r.stats.fail(err)
			}
			batch = nil
			return
		}
		for _, m := range batch {
			semaphore <- struct{}{}
			wg.Add(1)
			go func(m *machine) {
				defer wg.Done()
				defer func() { <-semaphore }()
				r.connect(m)
			}(m)
		}
		batch = nil
	}
	next := time.Now()
	for i := f.start; i < f.start+f.count && r.ctx.Err() == nil; i++ {
		m, err := r.build(i)
		if err != nil {
			r.stats.fail(err)
			continue
		}
		batch = append(batch, m)
		if len(batch) == f.batch {
			flush()
		}
		if interval > 0 {
			next = next.Add(interval)
			time.Sleep(time.Until(next))
		}
	}
	flush()
	wg.Wait()
}

func (r *runner) connect(m *machine) {
	r.stats.inflight.Add(1)
	defer r.stats.inflight.Add(-1)
	started := time.Now()
	var lastErr error
	for attempt := 0; attempt < 4 && r.ctx.Err() == nil; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		ctx, cancel := context.WithTimeout(r.ctx, 45*time.Second)
		err := r.attach(ctx, m)
		cancel()
		if err == nil {
			r.stats.connected.Add(1)
			r.stats.mu.Lock()
			r.stats.latencies = append(r.stats.latencies, time.Since(started))
			r.stats.mu.Unlock()
			return
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("interrupted")
	}
	r.stats.fail(lastErr)
}

// attach does what runtimeattachment.Service.connect does for one carrier: acquire it, put a hub on it and serve the runtime route and
// every endpoint route the way the agent's origin forwarder does, to the shared origin.
func (r *runner) attach(ctx context.Context, m *machine) error {
	session, err := m.source.AcquireRuntimeCarrier(ctx, m.admitted)
	if err != nil {
		return err
	}
	hub, err := preview.NewDataCarrierPreviewHub(r.ctx, preview.DataCarrierPreviewHubConfig{Active: session.Active, Identity: session.Identity,
		EdgeNodeID: r.edge.NodeID, EdgeProcessEpoch: r.edge.ProcessEpoch})
	if err != nil {
		_ = session.Release(context.Background())
		return err
	}
	// The inspector records on the machine, one store per daemon; the edge only forwards.
	var store *inspector.Store
	if r.f.inspector {
		store = inspector.NewStore()
		go store.Run(r.ctx)
	}
	serve := func(a runtimewire.Admission, engine bool) error {
		registration, err := hub.Register(a.Binding.RouteID)
		if err != nil {
			return err
		}
		route := hoststate.TunnelConfigRoute{ID: a.Binding.RouteID, Protocol: "http", MatchType: "exact", MatchHostname: a.Hostname, OriginScheme: "http", OriginAddress: r.f.origin,
			PreserveHost: true, TLSVerification: "not_applicable", ConnectTimeoutMs: 5000, IdleTimeoutMs: 3600000, MaxConcurrentStreams: 256, DesiredState: "active"}
		forwarder := tunnelmanager.OriginStreamForwarder{Transport: r.transport, EngineRoutes: engine}
		if store != nil {
			if err := store.SetPolicy(a.Binding.RouteID, inspector.ResourcePolicy{Enabled: true, CaptureRequestBody: true, CaptureResponseBody: true}); err != nil {
				return err
			}
			forwarder.Inspector = store
		}
		_, err = forwarder.StartFrom(r.ctx, registration, session.Identity, []hoststate.TunnelConfigRoute{route})
		return err
	}
	if err := serve(m.runtime, false); err != nil {
		_ = session.Release(context.Background())
		return err
	}
	for _, a := range m.routes {
		if err := serve(a, true); err != nil {
			_ = session.Release(context.Background())
			return err
		}
	}
	r.mu.Lock()
	r.releases = append(r.releases, session.Release)
	r.mu.Unlock()
	return nil
}

// serveOrigin is the one app every route forwards to: /small is a few bytes, /big?bytes=N streams N zero bytes.
func serveOrigin(address string) {
	zeros := make([]byte, 64<<10)
	mux := http.NewServeMux()
	mux.HandleFunc("/big", func(w http.ResponseWriter, req *http.Request) {
		n, _ := strconv.ParseInt(req.URL.Query().Get("bytes"), 10, 64)
		w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
		for n > 0 {
			chunk := min(n, int64(len(zeros)))
			if _, err := w.Write(zeros[:chunk]); err != nil {
				return
			}
			n -= chunk
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	server := &http.Server{Addr: address, Handler: mux}
	_ = server.ListenAndServe()
}
