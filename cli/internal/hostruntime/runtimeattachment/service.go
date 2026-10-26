// Package runtimeattachment connects an installed machine's loopback runtime to
// its server-authorized edge route. The edge never receives the loopback target.
package runtimeattachment

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
	"io"
	jitterrand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	lifetimeadmission "github.com/pinksaucepasta/paperboat/shared/admission"
	sharedprotocol "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/browserbroadcastserver"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/edgeplacement"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machinecontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

const path = "/v1/runtime/carrier-attachment"

type Config struct {
	ControlURL             string
	StateRoot              string
	Transport              http.RoundTripper
	MachineID              string
	WorkerGeneration       uint64
	InstallationGeneration uint64
	ListenAddress          string
}

type Service struct {
	config      Config
	auth        *machinecontrol.Source
	client      *http.Client
	sessions    *preview.MachineAttachmentSessionSource
	processID   string
	broadcaster *browserbroadcastserver.Registry
	selector    edgeplacement.Selector
	preference  *edgeplacement.Preference
	lastProbe   time.Time
	latency     []edgeLatency                       // edge probe times to send with the next control request
	probeDial   func(context.Context, string) error // tests only; nil dials TCP
	mu          sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}

	// hubs are the live carriers' stream hubs, by carrier key, for endpoint routes (PLAN 3a);
	// hubsChanged is closed and replaced whenever the set changes.
	hubsMu sync.Mutex
	hubs   map[string]*preview.DataCarrierPreviewHub
	// goaways tells each edge of a live carrier that it is going away (T10), keyed like hubs.
	goaways     map[string]func()
	hubsChanged chan struct{}
	served      bool
	// tunnelsServed: every edge of the attachment serves this agent's tunnels as endpoint routes
	// (PLAN 3a step 4); tunnelsChanged is closed and replaced when it changes.
	tunnelsServed  bool
	tunnelsChanged chan struct{}
	// answered is closed by the first successful attachment answer.
	answered     chan struct{}
	answeredOnce sync.Once
}

// EndpointsServed implements preview.AgentCarriers: every edge of the latest attachment admits
// endpoint routes. Until the first attachment answer after a start it waits (bounded by ctx), so
// a preview resumed right after a host restart does not fall back to its own carrier.
func (s *Service) EndpointsServed(ctx context.Context) bool {
	select {
	case <-s.answered:
	case <-ctx.Done():
		return false
	}
	s.hubsMu.Lock()
	defer s.hubsMu.Unlock()
	return s.served
}

// Hubs implements preview.AgentCarriers: the stream hubs of the runtime carriers this agent holds
// now, and a channel closed when that set changes.
func (s *Service) Hubs() ([]*preview.DataCarrierPreviewHub, <-chan struct{}) {
	s.hubsMu.Lock()
	defer s.hubsMu.Unlock()
	out := make([]*preview.DataCarrierPreviewHub, 0, len(s.hubs))
	for _, hub := range s.hubs {
		out = append(out, hub)
	}
	return out, s.hubsChanged
}

func (s *Service) setHub(key string, hub *preview.DataCarrierPreviewHub, goaway func()) {
	s.hubsMu.Lock()
	defer s.hubsMu.Unlock()
	if hub == nil {
		delete(s.hubs, key)
		delete(s.goaways, key)
	} else {
		s.hubs[key] = hub
		s.goaways[key] = goaway
	}
	close(s.hubsChanged)
	s.hubsChanged = make(chan struct{})
}

// BindBroadcaster connects browser output to this service's authenticated
// runtime carrier. It must be called before Start.
func (s *Service) BindBroadcaster(value *browserbroadcastserver.Registry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done != nil || s.broadcaster != nil || value == nil {
		return errors.New("runtime broadcaster binding invalid")
	}
	s.broadcaster = value
	return nil
}

type binding struct {
	AccountID                            string `json:"account_id"`
	HostID                               string `json:"host_id"`
	TunnelID                             string `json:"tunnel_id"`
	ConnectorID                          string `json:"connector_id"`
	SessionID                            string `json:"session_id"`
	ProcessGeneration                    uint64 `json:"process_generation"`
	ConfigGeneration                     uint64 `json:"config_generation"`
	RouteID                              string `json:"route_id"`
	InstallationGeneration               uint64 `json:"installation_generation"`
	EdgeNodeID                           string `json:"edge_node_id"`
	EdgeProcessEpoch                     string `json:"edge_process_epoch"`
	EdgeCarrierServerSPKISHA256          string `json:"edge_carrier_server_spki_sha256"`
	EdgeCarrierServerCertificateChainPEM string `json:"edge_carrier_server_certificate_chain_pem"`
	MachineIdentityPublicKey             string `json:"machine_identity_public_key"`
	MachineIdentityThumbprint            string `json:"machine_identity_thumbprint"`
}
type admission struct {
	Schema                 string                    `json:"schema"`
	Binding                binding                   `json:"binding"`
	EdgeEndpoints          []string                  `json:"edge_endpoints"`
	Hostname               string                    `json:"hostname"`
	RouteKind              string                    `json:"route_kind"`
	ExpiresAt              time.Time                 `json:"expires_at"`
	EdgeCandidates         []edgeplacement.Candidate `json:"edge_candidates"`
	Backup                 *admissionBinding         `json:"backup,omitempty"`
	ActiveEdgeNodeID       string                    `json:"active_edge_node_id,omitempty"`
	ActiveEdgeProcessEpoch string                    `json:"active_edge_process_epoch,omitempty"`
	EndpointsServed        bool                      `json:"endpoints_served,omitempty"`
	TunnelEndpointsServed  bool                      `json:"tunnel_endpoints_served,omitempty"`
}

type admissionBinding struct {
	Schema        string    `json:"schema"`
	Binding       binding   `json:"binding"`
	EdgeEndpoints []string  `json:"edge_endpoints"`
	Hostname      string    `json:"hostname"`
	RouteKind     string    `json:"route_kind"`
	ExpiresAt     time.Time `json:"expires_at"`
}
type live struct {
	key     string
	active  *connector.ActiveDataCarrier
	hub     *preview.DataCarrierPreviewHub
	streams *tunnelmanager.RunningOriginStreams
	release func(context.Context) error
	open    browserbroadcastserver.OpenPublisher
}

// attachmentCheckTimeout bounds one attachment reconciliation: the control
// request, an edge probe and any follow-up request.
const attachmentCheckTimeout = 10 * time.Second

type retryState struct {
	attempts int
	next     time.Time
	nodeID   string
	epoch    string
}

func New(config Config) (*Service, error) {
	u, err := url.Parse(config.ControlURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || config.StateRoot == "" || config.MachineID == "" || config.WorkerGeneration == 0 || config.InstallationGeneration == 0 || config.ListenAddress == "" {
		return nil, errors.New("invalid runtime attachment configuration")
	}
	host, portText, err := net.SplitHostPort(config.ListenAddress)
	port, portErr := strconv.Atoi(portText)
	if err != nil || portErr != nil || port < 1024 || port > 65535 || (host != "127.0.0.1" && host != "::1") {
		return nil, errors.New("runtime attachment requires loopback listener")
	}
	auth, err := machinecontrol.NewSource(machinecontrol.Config{ControlURL: config.ControlURL, StateRoot: config.StateRoot, Transport: config.Transport})
	if err != nil {
		return nil, err
	}
	carrierConfig := connector.DefaultDataCarrierPoolConfig()
	// Runtime attachments share one edge carrier; capacity comes from its
	// streams, not from opening a carrier for each browser participant.
	carrierConfig.MaximumCarriers = 1
	carrierConfig.Carrier.MaximumStreams = 256
	sessions, err := preview.NewMachineAttachmentSessionSource(preview.MachineAttachmentSessionSourceConfig{StateRoot: config.StateRoot, Carrier: carrierConfig})
	if err != nil {
		return nil, err
	}
	processID, err := randomID("process")
	if err != nil {
		return nil, err
	}
	return &Service{hubs: map[string]*preview.DataCarrierPreviewHub{}, goaways: map[string]func(){}, hubsChanged: make(chan struct{}), tunnelsChanged: make(chan struct{}), answered: make(chan struct{}), config: config, auth: auth, client: &http.Client{Transport: config.Transport, Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("runtime attachment redirect refused") }}, sessions: sessions, processID: processID}, nil
}

func (s *Service) Start(ctx context.Context) error {
	if ctx == nil {
		return errors.New("runtime attachment start context required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !supportref.Valid(supportref.FromContext(ctx)) {
		ctx = supportref.WithContext(ctx, supportref.New())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done != nil {
		return errors.New("runtime attachment already started")
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.done = make(chan struct{})
	go func() { defer close(s.done); s.run(runCtx) }()
	return nil
}

func (s *Service) Shutdown(ctx context.Context) error {
	// Each edge hears that its carrier is going away while the carriers still run; canceling
	// below closes them.
	s.sendGoaways()
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	done := s.done
	s.mu.Unlock()
	if done == nil {
		return s.sessions.Close(ctx)
	}
	select {
	case <-done:
		return s.sessions.Close(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) run(ctx context.Context) {
	lifetime, err := lifetimeadmission.Admit(ctx, time.Time{}, nil)
	if err != nil {
		return
	}
	defer lifetime.Close()
	ctx = lifetime.Context()
	lives := map[string]*live{}
	retries := map[string]retryState{}
	publisherKey := ""
	var lastFailure *failureClass
	var pacer roundPacer
	defer func() {
		var cleanupErr error
		if s.broadcaster != nil {
			s.broadcaster.SetPublisher(nil)
		}
		for _, item := range lives {
			cleanupErr = errors.Join(cleanupErr, s.closeLive(item))
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cleanupErr = errors.Join(cleanupErr, s.sessions.Close(cleanup))
		if cleanupErr != nil {
			failure := cleanupFailure(cleanupErr)
			fault := errorreport.ProjectFault(ctx, "paperboat-daemon", "preview_attachment", "lifecycle", "service_failed", failure)
			if fault.Outcome != "canceled" {
				errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", "preview_attachment", "lifecycle", "service_failed", failure)
			}
		}
	}()
	stopPoll := lifetime.Poll(func(context.Context) error {
		// Poll bounds its own context to the 1s revocation cadence. That is far
		// too short for a control round trip plus an edge probe, and an
		// unresponsive candidate would then fail the whole check on every poll
		// and keep the carrier from ever connecting. Each attachment check owns
		// a bounded context derived from the service lifetime instead.
		if !pacer.due(time.Now(), carriersReady(lives, publisherKey)) {
			return nil
		}
		checkCtx, cancelCheck := context.WithTimeout(ctx, attachmentCheckTimeout)
		defer cancelCheck()
		wantedCount := 0
		response, err := s.fetch(checkCtx, "", "")
		var probeErr error
		if err == nil && len(response.EdgeCandidates) != 0 && (s.lastProbe.IsZero() || time.Since(s.lastProbe) >= 30*time.Second) {
			measurements, probeFailure := edgeplacement.Probe(checkCtx, response.EdgeCandidates, s.probeDial)
			s.lastProbe = time.Now()
			s.latency = edgeLatencies(measurements)
			if probeFailure != nil {
				// Placement is an optimization: connect with the server's own
				// ordering and report the probe failure afterwards.
				probeErr = peerConnectFailure(probeFailure)
			} else {
				eligible := measurements[:0]
				for _, measurement := range measurements {
					failed := false
					for _, retry := range retries {
						if retry.nodeID == measurement.Candidate.NodeID && retry.epoch == measurement.Candidate.ProcessEpoch && time.Now().Before(retry.next) {
							failed = true
							break
						}
					}
					if !failed {
						eligible = append(eligible, measurement)
					}
				}
				measurements = eligible
			}
			if probeErr == nil && len(measurements) != 0 {
				selected, selectErr := s.selector.Select(s.lastProbe, measurements)
				if selectErr != nil {
					err = classifyFailure(selectErr, "reconciliation", "service_failed")
				} else if s.preference == nil || *s.preference != selected {
					s.preference = &selected
					response, err = s.fetch(checkCtx, "", "")
				}
			}
		}
		if err == nil {
			s.hubsMu.Lock()
			s.served = response.EndpointsServed
			if s.tunnelsServed != response.TunnelEndpointsServed {
				s.tunnelsServed = response.TunnelEndpointsServed
				close(s.tunnelsChanged)
				s.tunnelsChanged = make(chan struct{})
			}
			s.hubsMu.Unlock()
			s.answeredOnce.Do(func() { close(s.answered) })
			ordered := []admissionBinding{response.primary()}
			if response.Backup != nil {
				ordered = append(ordered, *response.Backup)
			}
			wanted := make(map[string]bool, len(ordered))
			for _, admission := range ordered {
				key := carrierKey(admission)
				wanted[key] = true
			}
			wantedCount = len(wanted)
			for key, item := range lives {
				if !wanted[key] || item.active.Pool().State() != connector.DataCarrierPoolReady {
					if publisherKey == key && s.broadcaster != nil {
						diagnosticlog.TryInfo("browser output publisher stopped: carrier unwanted or pool not ready", "wanted", wanted[key], "pool_state", string(item.active.Pool().State()))
						s.broadcaster.SetPublisher(nil)
						publisherKey = ""
					}
					if closeErr := s.closeLive(item); closeErr != nil {
						err = errors.Join(err, cleanupFailure(closeErr))
					}
					delete(lives, key)
				}
			}
			for _, admission := range ordered {
				key := carrierKey(admission)
				if item := lives[key]; item != nil {
					continue
				}
				if retry := retries[key]; time.Now().Before(retry.next) {
					continue
				}
				item, connectErr := s.connect(ctx, admission, key)
				if connectErr != nil {
					err = connectErr
					s.lastProbe = time.Time{}
					retry := retries[key]
					retry.nodeID, retry.epoch = admission.Binding.EdgeNodeID, admission.Binding.EdgeProcessEpoch
					if retry.attempts < 4 {
						retry.attempts++
					}
					delay := time.Duration(1<<retry.attempts)*time.Second + time.Duration(jitterrand.Int64N(int64(time.Second)))
					retry.next = time.Now().Add(delay)
					retries[key] = retry
					continue
				}
				delete(retries, key)
				lives[key] = item
			}
			for key := range retries {
				if !wanted[key] {
					delete(retries, key)
				}
			}
			chosen, chosenAdmission := chooseLive(ordered, lives, response.ActiveEdgeNodeID, response.ActiveEdgeProcessEpoch)
			if chosen != nil && (response.ActiveEdgeNodeID != chosenAdmission.Binding.EdgeNodeID || response.ActiveEdgeProcessEpoch != chosenAdmission.Binding.EdgeProcessEpoch) {
				updated, updateErr := s.fetch(checkCtx, chosenAdmission.Binding.EdgeNodeID, chosenAdmission.Binding.EdgeProcessEpoch)
				if updateErr == nil {
					response = updated
				} else {
					err = updateErr
				}
			}
			if chosen == nil || response.ActiveEdgeNodeID != chosenAdmission.Binding.EdgeNodeID || response.ActiveEdgeProcessEpoch != chosenAdmission.Binding.EdgeProcessEpoch {
				chosen = nil
				for _, candidate := range ordered {
					if candidate.Binding.EdgeNodeID == response.ActiveEdgeNodeID && candidate.Binding.EdgeProcessEpoch == response.ActiveEdgeProcessEpoch {
						chosen, chosenAdmission = lives[carrierKey(candidate)], candidate
						break
					}
				}
			}
			if chosen == nil || response.ActiveEdgeNodeID != chosenAdmission.Binding.EdgeNodeID || response.ActiveEdgeProcessEpoch != chosenAdmission.Binding.EdgeProcessEpoch {
				if publisherKey != "" && s.broadcaster != nil {
					diagnosticlog.TryInfo("browser output publisher stopped: no live carrier for the active edge", "chosen", chosen != nil)
					s.broadcaster.SetPublisher(nil)
					publisherKey = ""
				}
			} else if publisherKey != chosen.key {
				if s.broadcaster != nil {
					diagnosticlog.TryInfo("browser output publisher moved to another carrier", "had_publisher", publisherKey != "")
					s.broadcaster.SetPublisher(chosen.open)
				}
				publisherKey = chosen.key
			}
		}
		if err == nil && probeErr != nil {
			err = probeErr
		}
		for key, item := range lives {
			if item.active.Pool().State() == connector.DataCarrierPoolReady {
				continue
			}
			if publisherKey == key && s.broadcaster != nil {
				diagnosticlog.TryInfo("browser output publisher stopped: its carrier pool is not ready", "pool_state", string(item.active.Pool().State()))
				s.broadcaster.SetPublisher(nil)
				publisherKey = ""
			}
			if closeErr := s.closeLive(item); closeErr != nil {
				err = errors.Join(err, cleanupFailure(closeErr))
			}
			delete(lives, key)
		}
		expiresAt := response.ExpiresAt
		if response.Backup != nil && response.Backup.ExpiresAt.Before(expiresAt) {
			expiresAt = response.Backup.ExpiresAt
		}
		pacer.finished(time.Now(), err == nil && wantedCount > 0 && len(lives) == wantedCount && carriersReady(lives, publisherKey), expiresAt)
		if err != nil {
			s.lastProbe = time.Time{}
			lastFailure = s.observeFailure(ctx, err, lastFailure)
		} else if lastFailure != nil {
			errorreport.Current().Lifecycle(ctx, "edge", "preview_attachment", "recovered", "success")
			lastFailure = nil
		}
		return err
	})
	defer stopPoll()
	<-ctx.Done()
}

// carriersReady reports whether every connected carrier is ready and one of them publishes.
func carriersReady(lives map[string]*live, publisherKey string) bool {
	if len(lives) == 0 || publisherKey == "" {
		return false
	}
	for _, item := range lives {
		if item.active.Pool().State() != connector.DataCarrierPoolReady {
			return false
		}
	}
	return true
}

func chooseLive(ordered []admissionBinding, lives map[string]*live, activeNode, activeEpoch string) (*live, admissionBinding) {
	// Preserve the confirmed active carrier while it remains in the measured
	// pair. A recovered faster edge becomes a ready backup; moving the public
	// route immediately can strand browsers on their cached DNS address.
	for _, candidate := range ordered {
		if candidate.Binding.EdgeNodeID == activeNode && candidate.Binding.EdgeProcessEpoch == activeEpoch {
			if item := lives[carrierKey(candidate)]; item != nil {
				return item, candidate
			}
			break
		}
	}
	for _, candidate := range ordered {
		if item := lives[carrierKey(candidate)]; item != nil {
			return item, candidate
		}
	}
	return nil, admissionBinding{}
}

type failureClass struct {
	stage, code, cause string
	errno, status      int
}

func (s *Service) observeFailure(ctx context.Context, err error, previous *failureClass) *failureClass {
	fault := errorreport.ProjectFault(ctx, "paperboat-daemon", "preview_attachment", "lifecycle", "service_failed", err)
	if fault.Outcome == "canceled" {
		return previous
	}
	class := failureClass{stage: fault.Stage, code: fault.Code, cause: fault.Cause, errno: fault.Errno, status: fault.HTTPStatus}
	if previous != nil && *previous == class {
		return previous
	}
	if !errorreport.HTTPAttemptObserved(err) {
		errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "preview_attachment", "lifecycle", "service_failed", err)
	}
	return &class
}

func (s *Service) fetch(ctx context.Context, activeNode, activeEpoch string) (out admission, returnErr error) {
	op, err := randomID("operation")
	if err != nil {
		return out, controlFailure(err)
	}
	_, portText, _ := net.SplitHostPort(s.config.ListenAddress)
	port, _ := strconv.Atoi(portText) // validated by New
	request := map[string]any{"operation_id": op, "process_id": s.processID, "worker_generation": s.config.WorkerGeneration, "installation_generation": s.config.InstallationGeneration, "runtime_listen_port": port, "capabilities": []string{runtimewire.CapabilityEndpoints, runtimewire.CapabilityTunnelEndpoints}}
	if s.preference != nil {
		request["edge_preference"] = s.preference
	}
	if len(s.latency) != 0 {
		// The server shows these to the person; one report per probe is enough.
		request["edge_latency"], s.latency = s.latency, nil
	}
	if activeNode != "" {
		request["active_edge_node_id"], request["active_edge_process_epoch"] = activeNode, activeEpoch
	}
	res, err := s.post(ctx, path, op, request)
	if err != nil {
		return out, err
	}
	defer func() {
		if closeErr := res.Body.Close(); closeErr != nil {
			out = admission{}
			if returnErr != nil {
				closeErr = errors.Join(returnErr, closeErr)
			}
			returnErr = controlFailure(closeErr)
		}
	}()
	if res.StatusCode != http.StatusOK {
		_, drainErr := io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
		statusErr := errorreport.HTTPStatusFailure(res)
		if drainErr != nil {
			statusErr = errors.Join(statusErr, drainErr)
		}
		if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusNotFound {
			statusErr = errors.Join(lifetimeadmission.ErrRevoked, statusErr)
		}
		return out, controlFailure(statusErr)
	}
	var envelope struct {
		Data admission `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&envelope); err != nil {
		return out, controlFailure(errors.Join(preview.ErrMachineAttachmentSessionInvalid, err))
	}
	out = envelope.Data
	if out.Schema != "paperboat.runtime-carrier/v1" || out.Binding.HostID != s.config.MachineID || out.Binding.InstallationGeneration != s.config.InstallationGeneration || out.Binding.ProcessGeneration != s.config.WorkerGeneration || out.Binding.RouteID == "" || out.Binding.SessionID == "" || out.RouteKind != "runtime_https_wss" || !out.ExpiresAt.After(serverclock.Now()) {
		return admission{}, controlFailure(preview.ErrMachineAttachmentSessionInvalid)
	}
	if out.Backup != nil && (out.Backup.Schema != out.Schema || out.Backup.Binding.HostID != out.Binding.HostID || out.Backup.Binding.AccountID != out.Binding.AccountID || out.Backup.Binding.RouteID != out.Binding.RouteID || out.Backup.Binding.EdgeNodeID == out.Binding.EdgeNodeID || out.Backup.Binding.EdgeProcessEpoch == "" || out.Backup.Binding.InstallationGeneration != out.Binding.InstallationGeneration || out.Backup.Binding.ProcessGeneration != out.Binding.ProcessGeneration || out.Backup.RouteKind != out.RouteKind || !out.Backup.ExpiresAt.After(serverclock.Now())) {
		return admission{}, controlFailure(preview.ErrMachineAttachmentSessionInvalid)
	}
	return out, nil
}

// post sends one machine-signed control request.
func (s *Service) post(ctx context.Context, endpointPath, op string, request any) (*http.Response, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, controlFailure(err)
	}
	token, err := s.auth.Token(ctx)
	if err != nil {
		// A long-idle installation may have missed the renewal window. Its
		// enrolled machine identity can recover the control credential.
		token, err = s.auth.EnsureInitial(ctx)
		if err != nil {
			return nil, controlFailure(err)
		}
	}
	proof, err := s.auth.Proof(ctx, op, http.MethodPost, endpointPath, body)
	if err != nil {
		return nil, controlFailure(err)
	}
	endpoint := strings.TrimRight(s.config.ControlURL, "/") + endpointPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, controlFailure(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Paperboat-Machine-Identity", token)
	req.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	req.Header.Set("Content-Type", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return nil, controlFailure(err)
	}
	return res, nil
}

func (a admission) primary() admissionBinding {
	return admissionBinding{Schema: a.Schema, Binding: a.Binding, EdgeEndpoints: a.EdgeEndpoints, Hostname: a.Hostname, RouteKind: a.RouteKind, ExpiresAt: a.ExpiresAt}
}

func carrierKey(a admissionBinding) string {
	return a.Binding.SessionID + "/" + a.Binding.EdgeProcessEpoch + "/" + a.Binding.RouteID
}

func (s *Service) connect(ctx context.Context, a admissionBinding, key string) (*live, error) {
	b := a.Binding
	carrier, err := s.sessions.AcquireRuntimeCarrier(ctx, preview.RuntimeCarrierAdmission{AccountID: b.AccountID, MachineID: b.HostID, MachineIdentityPublicKey: b.MachineIdentityPublicKey, MachineIdentityThumbprint: b.MachineIdentityThumbprint, TunnelID: b.TunnelID, ConnectorID: b.ConnectorID, SessionID: b.SessionID, ProcessGeneration: b.ProcessGeneration, ConfigGeneration: b.ConfigGeneration, EdgeNodeID: b.EdgeNodeID, EdgeProcessEpoch: b.EdgeProcessEpoch, EdgeCarrierServerSPKISHA256: b.EdgeCarrierServerSPKISHA256, EdgeCarrierServerCertificateChainPEM: b.EdgeCarrierServerCertificateChainPEM, EdgeEndpoints: a.EdgeEndpoints, ExpiresAt: a.ExpiresAt})
	if err != nil {
		return nil, carrierFailure(err)
	}
	// One hub owns the carrier's inbound streams and routes them by route id: the runtime route
	// to the origin forwarder, endpoint routes (previews) to their route carriers.
	hub, err := preview.NewDataCarrierPreviewHub(ctx, preview.DataCarrierPreviewHubConfig{Active: carrier.Active, Identity: carrier.Identity, EdgeNodeID: b.EdgeNodeID, EdgeProcessEpoch: b.EdgeProcessEpoch})
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return nil, streamOpenFailure(errors.Join(err, carrier.Release(cleanup)))
	}
	registration, err := hub.Register(b.RouteID)
	if err != nil {
		_ = hub.Close()
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return nil, streamOpenFailure(errors.Join(err, carrier.Release(cleanup)))
	}
	forwarder := runtimeOriginForwarder()
	route := hoststate.TunnelConfigRoute{ID: b.RouteID, Protocol: "http", MatchType: "exact", MatchHostname: a.Hostname, OriginScheme: "http", OriginAddress: s.config.ListenAddress, PreserveHost: true, TLSVerification: "not_applicable", ConnectTimeoutMs: 5000, IdleTimeoutMs: 3600000, MaxConcurrentStreams: 256, DesiredState: "active"}
	streams, err := forwarder.StartFrom(ctx, registration, carrier.Identity, []hoststate.TunnelConfigRoute{route})
	if err != nil {
		_ = hub.Close()
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		releaseErr := carrier.Release(cleanup)
		return nil, streamOpenFailure(errors.Join(err, releaseErr))
	}
	opener := browserbroadcastserver.OpenPublisher(func(openCtx context.Context, terminalSessionID string) (io.WriteCloser, error) {
		if err := openCtx.Err(); err != nil {
			return nil, err
		}
		return carrier.Active.OpenStream(openCtx, connector.StreamOpen{
			Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: b.AccountID,
			TunnelID: b.TunnelID, ConnectorID: b.ConnectorID, SessionID: b.SessionID,
			ProcessGeneration: b.ProcessGeneration, Generation: b.ConfigGeneration,
			RouteID: b.RouteID, RequestID: terminalSessionID, Kind: "browser_terminal_output",
		})
	})
	goaway := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		requestID, err := randomID("request")
		if err != nil {
			return
		}
		stream, err := carrier.Active.OpenStream(ctx, connector.StreamOpen{
			Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: b.AccountID,
			TunnelID: b.TunnelID, ConnectorID: b.ConnectorID, SessionID: b.SessionID,
			ProcessGeneration: b.ProcessGeneration, Generation: b.ConfigGeneration,
			RouteID: b.RouteID, RequestID: requestID, Kind: sharedprotocol.Goaway,
		})
		if err != nil {
			return
		}
		defer stream.Close()
		if sharedprotocol.WriteGoawayNotice(stream, sharedprotocol.GoawayHostDraining, time.Now().Add(carrierDrainGrace)) != nil {
			return
		}
		// The edge closes the stream once it has the notice; wait for that (bounded by ctx) so the
		// carrier is not torn down first.
		received := make(chan struct{})
		go func() { _, _ = io.Copy(io.Discard, io.LimitReader(stream, 1)); close(received) }()
		select {
		case <-received:
		case <-ctx.Done():
		}
	}
	s.setHub(key, hub, goaway)
	return &live{key: key, active: carrier.Active, hub: hub, streams: streams, release: carrier.Release, open: opener}, nil
}

// runtimeOriginForwarder serves the machine's own runtime listener over the carrier. The control
// plane's requests to it (inbox listing, read and delete) carry X-Paperboat-* headers that the
// forwarder would strip for a user's origin.
func runtimeOriginForwarder() tunnelmanager.OriginStreamForwarder {
	return tunnelmanager.OriginStreamForwarder{Transport: &tunnelmanager.OriginHTTPTransport{KeepControlPlaneHeaders: true}}
}

// sendGoaways tells every edge of a live carrier, in parallel, that it is going away.
func (s *Service) sendGoaways() {
	s.hubsMu.Lock()
	goaways := make([]func(), 0, len(s.goaways))
	for _, goaway := range s.goaways {
		goaways = append(goaways, goaway)
	}
	s.hubsMu.Unlock()
	var sent sync.WaitGroup
	for _, goaway := range goaways {
		sent.Go(goaway)
	}
	sent.Wait()
}

// carrierDrainGrace is how long a closing carrier's open streams may finish.
const carrierDrainGrace = 5 * time.Second

func (s *Service) closeLive(l *live) error {
	if l == nil {
		return nil
	}
	// No goaway here: the carrier is shared and refcounted (previews hold it too), so retiring
	// this live does not close it; a goaway would leave the edge treating a carrier still in use as
	// going away (prod 2026-10-26). Shutdown sends the goaways, when every carrier closes.
	s.setHub(l.key, nil, nil)
	if l.hub != nil {
		_ = l.hub.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), carrierDrainGrace)
	defer cancel()
	var err error
	if l.streams != nil {
		err = errors.Join(err, l.streams.Close(ctx))
	}
	if l.release != nil {
		err = errors.Join(err, l.release(ctx))
	}
	return err
}

func randomID(noun string) (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return noun + "_" + id.String(), nil
}

// edgeLatencies turns successful probe times into the report the server stores. A probe that failed
// has no measurement and is simply absent.
func edgeLatencies(measurements []edgeplacement.Measurement) []edgeLatency {
	out := make([]edgeLatency, 0, len(measurements))
	for _, measurement := range measurements {
		millis := max(1, measurement.RTT.Milliseconds())
		if millis > maximumLatencyMilliseconds || len(out) == maximumEdgeLatencies {
			continue
		}
		out = append(out, edgeLatency{NodeID: measurement.Candidate.NodeID, ProcessEpoch: measurement.Candidate.ProcessEpoch, RTTMilliseconds: millis})
	}
	return out
}

// edgeLatency is the machine's connect time to one edge candidate, in the server's wire shape.
type edgeLatency struct {
	NodeID          string `json:"node_id"`
	ProcessEpoch    string `json:"process_epoch"`
	RTTMilliseconds int64  `json:"rtt_ms"`
}

// The server's bounds on one report: it ignores anything above them.
const (
	maximumLatencyMilliseconds = 10000
	maximumEdgeLatencies       = 32
)
