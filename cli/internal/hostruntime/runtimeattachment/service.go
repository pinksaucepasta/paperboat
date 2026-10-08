// Package runtimeattachment connects an installed machine's loopback runtime to
// its server-authorized edge route. The edge never receives the loopback target.
package runtimeattachment

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"io"
	jitterrand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/browserbroadcastserver"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/edgeplacement"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machinecontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
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
	mu          sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}
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
	key       string
	expiresAt time.Time
	active    *connector.ActiveDataCarrier
	streams   *tunnelmanager.RunningOriginStreams
	release   func(context.Context) error
	open      browserbroadcastserver.OpenPublisher
}

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
	return &Service{config: config, auth: auth, client: &http.Client{Transport: config.Transport, Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("runtime attachment redirect refused") }}, sessions: sessions, processID: processID}, nil
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
	lives := map[string]*live{}
	retries := map[string]retryState{}
	publisherKey := ""
	var lastFailure *failureClass
	defer func() {
		var cleanupErr error
		if s.broadcaster != nil {
			s.broadcaster.SetPublisher(nil)
		}
		for _, item := range lives {
			cleanupErr = errors.Join(cleanupErr, closeLive(item))
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
	for ctx.Err() == nil {
		response, err := s.fetch(ctx, "", "")
		if err == nil && len(response.EdgeCandidates) != 0 && (s.lastProbe.IsZero() || time.Since(s.lastProbe) >= 30*time.Second) {
			measurements, probeErr := edgeplacement.Probe(ctx, response.EdgeCandidates, nil)
			s.lastProbe = time.Now()
			if probeErr != nil {
				err = peerConnectFailure(probeErr)
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
			if err == nil && len(measurements) != 0 {
				selected, selectErr := s.selector.Select(s.lastProbe, measurements)
				if selectErr != nil {
					err = classifyFailure(selectErr, "reconciliation", "service_failed")
				} else if s.preference == nil || *s.preference != selected {
					s.preference = &selected
					response, err = s.fetch(ctx, "", "")
				}
			}
		}
		if err == nil {
			ordered := []admissionBinding{response.primary()}
			if response.Backup != nil {
				ordered = append(ordered, *response.Backup)
			}
			wanted := make(map[string]bool, len(ordered))
			for _, admission := range ordered {
				key := carrierKey(admission)
				wanted[key] = true
			}
			for key, item := range lives {
				if !wanted[key] || item.active.Pool().State() != connector.DataCarrierPoolReady || time.Now().After(item.expiresAt) {
					if publisherKey == key && s.broadcaster != nil {
						s.broadcaster.SetPublisher(nil)
						publisherKey = ""
					}
					if closeErr := closeLive(item); closeErr != nil {
						err = errors.Join(err, cleanupFailure(closeErr))
					}
					delete(lives, key)
				}
			}
			for _, admission := range ordered {
				key := carrierKey(admission)
				if item := lives[key]; item != nil {
					item.expiresAt = admission.ExpiresAt
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
				updated, updateErr := s.fetch(ctx, chosenAdmission.Binding.EdgeNodeID, chosenAdmission.Binding.EdgeProcessEpoch)
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
					s.broadcaster.SetPublisher(nil)
					publisherKey = ""
				}
			} else if publisherKey != chosen.key {
				if s.broadcaster != nil {
					s.broadcaster.SetPublisher(chosen.open)
				}
				publisherKey = chosen.key
			}
		}
		for key, item := range lives {
			if time.Now().Before(item.expiresAt) && item.active.Pool().State() == connector.DataCarrierPoolReady {
				continue
			}
			if publisherKey == key && s.broadcaster != nil {
				s.broadcaster.SetPublisher(nil)
				publisherKey = ""
			}
			if closeErr := closeLive(item); closeErr != nil {
				err = errors.Join(err, cleanupFailure(closeErr))
			}
			delete(lives, key)
		}
		if err != nil {
			s.lastProbe = time.Time{}
			lastFailure = s.observeFailure(ctx, err, lastFailure)
		} else if lastFailure != nil {
			errorreport.Current().Lifecycle(ctx, "edge", "preview_attachment", "recovered", "success")
			lastFailure = nil
		}
		wait := 20 * time.Second
		if err != nil {
			wait = 5 * time.Second
		}
		wait += time.Duration(jitterrand.Int64N(int64(time.Second)))
		for _, item := range lives {
			if until := time.Until(item.expiresAt); until < wait {
				wait = until
			}
		}
		if wait < time.Second {
			wait = time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
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

func (s *Service) fetch(ctx context.Context, activeNode, activeEpoch string) (admission, error) {
	var out admission
	op, err := randomID("operation")
	if err != nil {
		return out, controlFailure(err)
	}
	_, portText, _ := net.SplitHostPort(s.config.ListenAddress)
	port, _ := strconv.Atoi(portText) // validated by New
	request := map[string]any{"operation_id": op, "process_id": s.processID, "worker_generation": s.config.WorkerGeneration, "installation_generation": s.config.InstallationGeneration, "runtime_listen_port": port}
	if s.preference != nil {
		request["edge_preference"] = s.preference
	}
	if activeNode != "" {
		request["active_edge_node_id"], request["active_edge_process_epoch"] = activeNode, activeEpoch
	}
	body, err := json.Marshal(request)
	if err != nil {
		return out, controlFailure(err)
	}
	token, err := s.auth.Token(ctx)
	if err != nil {
		// A long-idle installation may have missed the renewal window. Its
		// enrolled machine identity can recover the control credential.
		token, err = s.auth.EnsureInitial(ctx)
		if err != nil {
			return out, controlFailure(err)
		}
	}
	proof, err := s.auth.Proof(ctx, op, http.MethodPost, path, body)
	if err != nil {
		return out, controlFailure(err)
	}
	endpoint := strings.TrimRight(s.config.ControlURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return out, controlFailure(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Paperboat-Machine-Identity", token)
	req.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	req.Header.Set("Content-Type", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return out, controlFailure(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		_, _ = io.CopyN(io.Discard, res.Body, 4096)
		return out, controlFailure(errorreport.HTTPStatusFailure(res))
	}
	var envelope struct {
		Data admission `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&envelope); err != nil {
		return out, controlFailure(errors.Join(preview.ErrMachineAttachmentSessionInvalid, err))
	}
	out = envelope.Data
	if out.Schema != "paperboat.runtime-carrier/v1" || out.Binding.HostID != s.config.MachineID || out.Binding.InstallationGeneration != s.config.InstallationGeneration || out.Binding.ProcessGeneration != s.config.WorkerGeneration || out.Binding.RouteID == "" || out.Binding.SessionID == "" || out.RouteKind != "runtime_https_wss" || !out.ExpiresAt.After(time.Now()) {
		return admission{}, controlFailure(preview.ErrMachineAttachmentSessionInvalid)
	}
	if out.Backup != nil && (out.Backup.Schema != out.Schema || out.Backup.Binding.HostID != out.Binding.HostID || out.Backup.Binding.AccountID != out.Binding.AccountID || out.Backup.Binding.RouteID != out.Binding.RouteID || out.Backup.Binding.EdgeNodeID == out.Binding.EdgeNodeID || out.Backup.Binding.EdgeProcessEpoch == "" || out.Backup.Binding.InstallationGeneration != out.Binding.InstallationGeneration || out.Backup.Binding.ProcessGeneration != out.Binding.ProcessGeneration || out.Backup.RouteKind != out.RouteKind || !out.Backup.ExpiresAt.After(time.Now())) {
		return admission{}, controlFailure(preview.ErrMachineAttachmentSessionInvalid)
	}
	return out, nil
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
	forwarder := tunnelmanager.OriginStreamForwarder{Transport: &tunnelmanager.OriginHTTPTransport{}}
	route := hoststate.TunnelConfigRoute{ID: b.RouteID, Protocol: "http", MatchType: "exact", MatchHostname: a.Hostname, OriginScheme: "http", OriginAddress: s.config.ListenAddress, PreserveHost: true, TLSVerification: "not_applicable", ConnectTimeoutMs: 5000, IdleTimeoutMs: 3600000, MaxConcurrentStreams: 256, DesiredState: "active"}
	streams, err := forwarder.Start(ctx, carrier.Active, []hoststate.TunnelConfigRoute{route})
	if err != nil {
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
	return &live{key: key, expiresAt: a.ExpiresAt, active: carrier.Active, streams: streams, release: carrier.Release, open: opener}, nil
}

func closeLive(l *live) error {
	if l == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
