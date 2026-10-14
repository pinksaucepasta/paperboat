package tunnelmanager

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/inspectorapi"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/inspectorauth"
	"github.com/pinksaucepasta/paperboat/internal/inspector"
)

type task32OwnerCarrier struct {
	AccountID         string `json:"account_id"`
	MachineID         string `json:"machine_id"`
	TunnelID          string `json:"tunnel_id"`
	ConnectorID       string `json:"connector_id"`
	SessionID         string `json:"session_id"`
	RouteID           string `json:"route_id"`
	ProcessGeneration uint64 `json:"process_generation"`
	Generation        uint64 `json:"generation"`
}
type task32OwnerAccess struct {
	ResourceGeneration uint64             `json:"resource_generation"`
	RouteGeneration    uint64             `json:"route_generation"`
	TargetGeneration   uint64             `json:"target_generation"`
	Carrier            task32OwnerCarrier `json:"carrier"`
}
type task32OwnerDescriptor struct {
	BaseURL                                                                                     string `json:"base_url"`
	PreviewID                                                                                   string `json:"preview_id"`
	TunnelID                                                                                    string `json:"tunnel_id"`
	RouteID                                                                                     string `json:"route_id"`
	MachineID, EnvironmentID, MachineToken, MachinePrivateKey, TLSCertificatePEM, OriginAddress string
	MachineGeneration                                                                           int64
	PreviewCarrier, TunnelCarrier                                                               task32OwnerCarrier
	PreviewAccess, TunnelAccess                                                                 task32OwnerAccess
}
type task32OwnerEdgeReady struct {
	PreviewCarrierAddress string `json:"preview_carrier_address"`
	TunnelCarrierAddress  string `json:"tunnel_carrier_address"`
}
type task32OwnerReady struct {
	OriginAddress string `json:"origin_address"`
	EvidencePath  string `json:"evidence_path"`
	ReseedPath    string `json:"reseed_path"`
}
type task32OriginObservation struct {
	Method     string `json:"method"`
	RequestURI string `json:"request_uri"`
	BodySHA256 string `json:"body_sha256"`
}

// TestTask32LinkedOwnerFixture runs a real shared daemon inspector service and
// two production-compatible owner carrier clients. Live authorization always
// uses the enrolled machine proof supplied by the PG authority fixture.
func TestTask32LinkedOwnerFixture(t *testing.T) {
	descriptorPath, edgeReadyPath := strings.TrimSpace(os.Getenv("PAPERBOAT_TASK32_DESCRIPTOR")), strings.TrimSpace(os.Getenv("PAPERBOAT_TASK32_EDGE_READY"))
	readyPath, stopPath := strings.TrimSpace(os.Getenv("PAPERBOAT_TASK32_OWNER_READY")), strings.TrimSpace(os.Getenv("PAPERBOAT_TASK32_STOP"))
	evidencePath := strings.TrimSpace(os.Getenv("PAPERBOAT_TASK32_EVIDENCE"))
	reseedPath := strings.TrimSpace(os.Getenv("PAPERBOAT_TASK32_RESEED"))
	nativeOnly := os.Getenv("PAPERBOAT_TASK32_NATIVE_READY") != ""
	if descriptorPath == "" || (!nativeOnly && edgeReadyPath == "") || readyPath == "" || stopPath == "" || evidencePath == "" || reseedPath == "" {
		t.Skip("set task32 descriptor, ready and stop paths")
	}
	var d task32OwnerDescriptor
	task32ReadPrivateJSON(t, descriptorPath, &d)
	var edge task32OwnerEdgeReady
	if !nativeOnly {
		task32ReadPrivateJSON(t, edgeReadyPath, &edge)
	}
	private, err := base64.RawURLEncoding.DecodeString(d.MachinePrivateKey)
	if err != nil || len(private) != ed25519.PrivateKeySize {
		t.Fatal("invalid fixture machine key")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(d.TLSCertificatePEM)) {
		t.Fatal("invalid fixture CA")
	}
	authorize, err := (inspectorauth.Config{BaseURL: d.BaseURL, Source: &task32MachineSource{token: d.MachineToken, machine: d.MachineID, environment: d.EnvironmentID, generation: d.MachineGeneration, private: ed25519.PrivateKey(private)}, Client: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}}, Timeout: 15 * time.Second}}).AuthorizeFunc()
	if err != nil {
		t.Fatal(err)
	}
	store, registry := inspector.NewStore(), inspector.NewRegistry()
	service, err := inspectorapi.New(inspectorapi.Config{Store: store, Registry: registry, Authorize: authorize})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err = service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer service.Shutdown(context.Background())
	originListener, err := net.Listen("tcp", d.OriginAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer originListener.Close()
	var mu sync.Mutex
	var observations []task32OriginObservation
	origin := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		digest := sha256.Sum256(raw)
		mu.Lock()
		observations = append(observations, task32OriginObservation{Method: r.Method, RequestURI: r.URL.RequestURI(), BodySHA256: base64.RawURLEncoding.EncodeToString(digest[:])})
		evidence := append([]task32OriginObservation(nil), observations...)
		mu.Unlock()
		rawEvidence, _ := json.Marshal(evidence)
		if file, openErr := os.OpenFile(evidencePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600); openErr == nil {
			_, _ = file.Write(rawEvidence)
			_ = file.Close()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"token":"origin-secret"}`)
	}), ReadHeaderTimeout: 5 * time.Second}
	go origin.Serve(originListener)
	defer origin.Shutdown(context.Background())
	seed := func(resource, routeID string, access task32OwnerAccess) {
		if access.ResourceGeneration == 0 {
			t.Fatal("fixture missing exact inspector access generations")
		}
		if err := store.SetPolicy(resource, inspector.ResourcePolicy{Enabled: true, CaptureRequestBody: true, CaptureResponseBody: true, CaptureRaw: true}); err != nil {
			t.Fatal(err)
		}
		route := hoststate.TunnelConfigRoute{ID: routeID, Protocol: "http", OriginScheme: "http", OriginAddress: originListener.Addr().String(), TLSVerification: "not_applicable", ConnectTimeoutMs: 1000, IdleTimeoutMs: 5000, MaxConcurrentStreams: 4, DesiredState: "active"}
		identity := CaptureIdentity{ResourceID: resource, ResourceGeneration: access.ResourceGeneration, RouteGeneration: access.RouteGeneration, TargetGeneration: access.TargetGeneration, ExpiresAt: time.Now().Add(time.Hour)}
		forwarder := OriginStreamForwarder{Transport: &OriginHTTPTransport{}, Inspector: store, Registry: registry}
		daemon, client := net.Pipe()
		done := make(chan error, 1)
		go func() { done <- forwarder.ServePublicHTTP(ctx, daemon, bufio.NewReader(daemon), route, identity) }()
		req, _ := http.NewRequest(http.MethodPost, "http://fixture.invalid/task32?token=secret", strings.NewReader(`{"kind":"task32","password":"hidden"}`))
		req.Header.Set("Authorization", "Bearer hidden")
		req.Header.Set("Content-Type", "application/json")
		_ = req.Write(client)
		response, e := http.ReadResponse(bufio.NewReader(client), req)
		if e != nil {
			t.Fatal(e)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		_ = client.Close()
		if e = <-done; e != nil {
			t.Fatal(e)
		}
	}
	reseed := func() {
		seed(d.PreviewID, d.PreviewID, d.PreviewAccess)
		seed(d.RouteID, d.RouteID, d.TunnelAccess)
	}
	reseed()
	if nativeOnly {
		startInspectorNativeFixture(t, d, service)
	}
	serveCarrier := func(address string, identity task32OwnerCarrier) {
		connection, e := net.Dial("tcp", address)
		if e != nil {
			t.Fatal(e)
		}
		config := connector.DefaultDataCarrierConfig()
		carrier, e := connector.NewDataCarrierClient(ctx, connection, config, connector.DataCarrierIdentity{AccountID: identity.AccountID, HostID: identity.MachineID, TunnelID: identity.TunnelID, ConnectorID: identity.ConnectorID, SessionID: identity.SessionID, ProcessGeneration: identity.ProcessGeneration, Generation: identity.Generation})
		if e != nil {
			t.Fatal(e)
		}
		go func() {
			for {
				stream, open, e := carrier.AcceptStream(ctx)
				if e != nil {
					return
				}
				if open.Validate() != nil {
					open, e = connectorprotocol.ReadStreamOpen(stream)
					if e != nil {
						_ = stream.Close()
						continue
					}
				}
				go func() {
					defer stream.Close()
					if validateErr := open.Validate(); validateErr != nil || open.Kind != connectorprotocol.InspectorHTTP {
						_, _ = fmt.Fprintf(os.Stderr, "task32 inspector metadata invalid: validate=%v kind=%q route=%q request=%q generation=%d process=%d\n", validateErr, open.Kind, open.RouteID, open.RequestID, open.Generation, open.ProcessGeneration)
					}
					if serveErr := ServeInspectorStream(ctx, stream, open, service); serveErr != nil {
						_, _ = fmt.Fprintf(os.Stderr, "task32 inspector stream failed: %v\n", serveErr)
					}
				}()
			}
		}()
	}
	if !nativeOnly {
		serveCarrier(edge.PreviewCarrierAddress, d.PreviewCarrier)
		serveCarrier(edge.TunnelCarrierAddress, d.TunnelCarrier)
	}
	task32WritePrivateJSON(t, readyPath, task32OwnerReady{OriginAddress: originListener.Addr().String(), EvidencePath: evidencePath, ReseedPath: reseedPath})
	defer os.Remove(readyPath)
	defer os.Remove(evidencePath)
	for {
		if _, err := os.Stat(stopPath); err == nil {
			return
		}
		if info, err := os.Stat(reseedPath); err == nil {
			if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
				t.Fatal("task32 reseed trigger must be a regular 0600 file")
			}
			if err = os.Remove(reseedPath); err != nil {
				t.Fatal(err)
			}
			reseed()
		}
		time.Sleep(200 * time.Millisecond)
	}
}

type task32MachineSource struct {
	token, machine, environment string
	generation                  int64
	private                     ed25519.PrivateKey
}

func (s *task32MachineSource) Token(context.Context) (string, error) {
	if len(s.token) < 32 {
		return "", errors.New("invalid token")
	}
	return s.token, nil
}
func (s *task32MachineSource) Proof(_ context.Context, operationID, method, path string, body []byte) ([]byte, error) {
	now := time.Now().UTC()
	digest := sha256.Sum256(body)
	payload, err := json.Marshal(struct {
		MachineID              string    `json:"machine_id"`
		EnvironmentID          string    `json:"environment_id"`
		InstallationGeneration int64     `json:"installation_generation"`
		OperationID            string    `json:"operation_id"`
		Method                 string    `json:"method"`
		Path                   string    `json:"path"`
		BodySHA256             string    `json:"body_sha256"`
		IssuedAt               time.Time `json:"issued_at"`
		ExpiresAt              time.Time `json:"expires_at"`
	}{s.machine, s.environment, s.generation, operationID, strings.ToUpper(method), path, base64.RawURLEncoding.EncodeToString(digest[:]), now, now.Add(time.Minute)})
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Algorithm string `json:"alg"`
		Payload   string `json:"payload"`
		Signature string `json:"signature"`
	}{"EdDSA", base64.RawURLEncoding.EncodeToString(payload), base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.private, payload))})
}
func task32ReadPrivateJSON(t *testing.T, path string, out any) {
	t.Helper()
	info, e := os.Stat(path)
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatal("task32 descriptor must be regular 0600")
	}
	raw, e := os.ReadFile(path)
	if e != nil || json.Unmarshal(raw, out) != nil {
		t.Fatal("invalid task32 descriptor")
	}
}
func task32WritePrivateJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, e := json.Marshal(value)
	if e != nil {
		t.Fatal(e)
	}
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.Write(raw); e != nil {
		_ = f.Close()
		t.Fatal(e)
	}
	if e = f.Close(); e != nil {
		t.Fatal(e)
	}
}
