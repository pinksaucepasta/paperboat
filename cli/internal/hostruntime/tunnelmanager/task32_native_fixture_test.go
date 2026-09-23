package tunnelmanager

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/inspectorapi"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/inspectorauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

type task32NativeDescriptor struct {
	OwnerAccountID, CLIAccessToken, CLIRefreshToken, CLIClientSessionID, NetworkIssuer string
	CLITokenExpiresAt                                                                  time.Time
	RelayNodeID, RelayProcessEpoch, RelayAddress, TLSPrivateKeyPEM                     string
}
type task32NativeKeys map[string]ed25519.PublicKey

func (k task32NativeKeys) Lookup(_ context.Context, id string) (ed25519.PublicKey, bool, error) {
	v, ok := k[id]
	return v, ok, nil
}

// startInspectorNativeFixture uses the real device token, endpoint enrollment,
// signed network admission and machine proof handlers of the linked authority.
func startInspectorNativeFixture(t *testing.T, d task32OwnerDescriptor, service *inspectorapi.Service) {
	t.Helper()
	ready := os.Getenv("PAPERBOAT_TASK32_NATIVE_READY")
	if ready == "" {
		return
	}
	var extra task32NativeDescriptor
	task32ReadPrivateJSON(t, os.Getenv("PAPERBOAT_TASK32_DESCRIPTOR"), &extra)
	if extra.CLIAccessToken == "" || extra.NetworkIssuer != d.BaseURL {
		t.Fatal("native fixture requires matching production authority issuer and CLI token")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(d.TLSCertificatePEM)) {
		t.Fatal("invalid native fixture CA")
	}
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}}, Timeout: 15 * time.Second}
	t.Cleanup(httpClient.CloseIdleConnections)
	dir := filepath.Join(filepath.Dir(ready), "native-profile")
	store := config.ProfileStore{Path: dir, Secrets: config.FileSecretStore{Dir: filepath.Join(dir, "secrets")}}
	cred := config.Credential{AccessToken: extra.CLIAccessToken, RefreshToken: extra.CLIRefreshToken, TokenType: "Bearer", ExpiresAt: extra.CLITokenExpiresAt}
	if profile, e := store.Load(d.BaseURL); errors.Is(e, config.ErrNoCredentials) {
		if err := store.Save(config.Profile{Issuer: d.BaseURL, Account: config.Account{ID: extra.OwnerAccountID}, CLIClientSessionID: extra.CLIClientSessionID}, cred); err != nil {
			t.Fatal(err)
		}
	} else if e != nil || profile.CLIClientSessionID != extra.CLIClientSessionID {
		t.Fatal("native fixture existing profile differs")
	}
	client := api.New(d.BaseURL, cred, httpClient)
	cliKeys, err := store.FreshPeerIdentityKeys(d.BaseURL, extra.OwnerAccountID, extra.CLIClientSessionID)
	if err != nil {
		t.Fatal(err)
	}
	root := cliKeys.RootPrivate.Public().(ed25519.PublicKey)
	if err = store.SavePeerAccountRootPublic(d.BaseURL, extra.OwnerAccountID, root); err != nil {
		t.Fatal(err)
	}
	issued := time.Now().UTC().Truncate(time.Second)
	if saved, e := store.LoadPeerCertificate(d.BaseURL, extra.CLIClientSessionID); e == nil {
		cert, e := endpointidentity.Parse(saved.Raw)
		if e != nil {
			t.Fatal(e)
		}
		issued = cert.Claims.IssuedAt
	}
	makeCertificate := func(endpoint string, role endpointidentity.Role, keys config.PeerIdentityKeys, serial uint64) (endpointidentity.Certificate, api.EndpointCertificateDocument) {
		certificate, e := endpointidentity.Sign(cliKeys.RootPrivate, endpointidentity.Claims{AccountID: extra.OwnerAccountID, Role: role, EndpointID: endpoint, NoisePublicKey: keys.NoisePublic, QUICPublicKey: keys.QUICPrivate.Public().(ed25519.PublicKey), Generation: 1, Serial: serial, IssuedAt: issued, ExpiresAt: issued.Add(time.Hour)})
		if e != nil {
			t.Fatal(e)
		}
		raw, e := certificate.MarshalBinary()
		if e != nil {
			t.Fatal(e)
		}
		hash := sha256.Sum256(root)
		roleName := "cli"
		if role == endpointidentity.RoleMachine {
			roleName = "machine"
		}
		doc := api.EndpointCertificateDocument{Version: 1, AccountID: extra.OwnerAccountID, KeyID: "aek_" + hex.EncodeToString(hash[:]), EndpointID: endpoint, Role: roleName, Generation: 1, Serial: serial, IssuedAt: issued.Format(time.RFC3339), ExpiresAt: issued.Add(time.Hour).Format(time.RFC3339), Certificate: base64.RawURLEncoding.EncodeToString(raw), CertificateFingerprint: certificate.Fingerprint()}
		if _, e = store.SavePeerCertificate(d.BaseURL, endpoint, raw); e != nil {
			t.Fatal(e)
		}
		return certificate, doc
	}
	_, cliDoc := makeCertificate(extra.CLIClientSessionID, endpointidentity.RoleCLI, cliKeys, 1)
	if _, err = client.BootstrapE2EEFresh(ctx, "native-fixture-bootstrap-"+extra.CLIClientSessionID, api.E2EEBootstrapInput{RootPublicKey: base64.RawURLEncoding.EncodeToString(root), Certificate: cliDoc}); err != nil {
		t.Fatalf("native CLI endpoint bootstrap: %v", err)
	}
	machineKeys, err := store.PeerEndpointKeys(d.BaseURL, extra.OwnerAccountID, d.MachineID)
	if err != nil {
		t.Fatal(err)
	}
	rawPrivate, err := base64.RawURLEncoding.DecodeString(d.MachinePrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	source := &task32MachineSource{token: d.MachineToken, machine: d.MachineID, environment: d.EnvironmentID, generation: d.MachineGeneration, private: ed25519.PrivateKey(rawPrivate)}
	operation := "native-fixture-machine-" + extra.CLIClientSessionID
	payload, _ := json.Marshal(map[string]any{"operation_id": operation, "generation": 1, "noise_public_key": base64.RawURLEncoding.EncodeToString(machineKeys.NoisePublic[:]), "quic_public_key": base64.RawURLEncoding.EncodeToString(machineKeys.QUICPrivate.Public().(ed25519.PublicKey))})
	path := "/v1/machine-peer-identity"
	proof, err := source.Proof(ctx, operation, http.MethodPost, path, payload)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, d.BaseURL+path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.MachineToken)
	req.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	response, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("native machine endpoint request status=%d", response.StatusCode)
	}
	machineCertificate, machineDoc := makeCertificate(d.MachineID, endpointidentity.RoleMachine, machineKeys, 1)
	if _, err = client.RegisterEndpointCertificate(ctx, "native-fixture-approve-"+extra.CLIClientSessionID, machineDoc); err != nil {
		t.Fatalf("native machine endpoint approval: %v", err)
	}
	leaf, err := endpointidentity.NewTLSCertificate(machineCertificate, root, machineKeys.QUICPrivate, issued, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, d.BaseURL+"/.well-known/jwks.json", nil)
	response, err = httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var jwks struct {
		Keys []struct {
			ID string `json:"kid"`
			X  string `json:"x"`
		}
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&jwks)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	keys := task32NativeKeys{}
	for _, key := range jwks.Keys {
		raw, e := base64.RawURLEncoding.DecodeString(key.X)
		if e != nil || len(raw) != 32 {
			t.Fatal("invalid native fixture JWKS")
		}
		keys[key.ID] = ed25519.PublicKey(raw)
	}
	relay, err := derpquic.NewServer(derpquic.Verifier{Issuer: d.BaseURL, NodeID: extra.RelayNodeID, NodeGeneration: 1, ProcessEpoch: extra.RelayProcessEpoch, Keys: keys})
	if err != nil {
		t.Fatal(err)
	}
	socket, err := net.ListenPacket("udp4", extra.RelayAddress)
	if err != nil {
		t.Fatal(err)
	}
	relayCertificate, err := tls.X509KeyPair([]byte(d.TLSCertificatePEM), []byte(extra.TLSPrivateKeyPEM))
	if err != nil {
		t.Fatal(err)
	}
	relayDone := make(chan error, 1)
	go func() {
		relayDone <- relay.Serve(ctx, socket, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{relayCertificate}})
	}()
	t.Cleanup(func() { cancel(); relay.Close(); socket.Close(); <-relayDone; relay.Wait() })
	authority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: store, Issuer: d.BaseURL, Keys: keys, Self: tailnet.NetworkBinding{AccountID: extra.OwnerAccountID, EndpointID: d.MachineID, Role: "machine", MachineID: d.MachineID, EndpointGeneration: 1, MachineGeneration: uint64(d.MachineGeneration), QUICCertificateFingerprint: machineCertificate.Fingerprint(), QUICPublicKey: base64.RawURLEncoding.EncodeToString(machineCertificate.Claims.QUICPublicKey)}})
	if err != nil {
		t.Fatal(err)
	}
	machineClient := api.New(d.BaseURL, config.Credential{}, httpClient)
	machineClient.SetMachineAuth(source)
	if err = authority.Register(ctx, machineClient, false); err != nil {
		t.Fatalf("native machine network registration: %v", err)
	}
	regions, err := authority.ConfigureRegionalRelays(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{leaf}})
	if err != nil || len(regions) == 0 {
		t.Fatalf("native regional admission: %v", err)
	}
	owner, err := native.NewOwner(native.Config{Authority: authority, TLS: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, NextProtos: []string{peerquic.ALPN}, Certificates: []tls.Certificate{leaf}}, RefreshAuthority: func(c context.Context) error { return authority.Refresh(c, machineClient) }})
	if err != nil {
		t.Fatal(err)
	} // Native validates signed endpoint/network identity itself.
	authorize, err := (inspectorauth.Config{BaseURL: d.BaseURL, Source: source, Client: httpClient}).AuthorizeFunc()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = authority.Listen(regions[0]); err != nil {
		t.Fatal(err)
	}
	ownerDone := make(chan error, 1)
	go func() {
		ownerDone <- owner.Listen(ctx, regions[0], func(c context.Context, session *native.Session) error {
			for {
				stream, header, e := session.AcceptAuthorized(c, func(c context.Context, h streamauth.Header) (string, error) {
					var target struct {
						ResourceKind string `json:"resource_kind"`
						ResourceID   string `json:"resource_id"`
						RouteID      string `json:"route_id"`
						Action       string `json:"action"`
					}
					if h.Consumer != "inspector" || json.Unmarshal([]byte(h.Target), &target) != nil {
						return "", errors.New("invalid inspector stream")
					}
					decision, e := authorize(c, h.Credential, target.ResourceKind, target.ResourceID, target.RouteID, target.Action)
					if e != nil {
						return "", e
					}
					if decision.CredentialID != h.OperationID {
						return "", errors.New("inspector operation mismatch")
					}
					return decision.CredentialID, nil
				})
				if e != nil {
					return e
				}
				go func(conn net.Conn) {
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(45 * time.Second))
					request, readErr := http.ReadRequest(bufio.NewReaderSize(conn, 16<<10))
					if readErr != nil {
						return
					}
					defer request.Body.Close()
					if request.Header.Get("X-Paperboat-Inspector-Grant") != header.Credential {
						return
					}
					request = request.WithContext(c)
					request.Body = http.MaxBytesReader(nil, request.Body, 64<<10)
					recorder := httptest.NewRecorder()
					service.ServeAuthenticatedHTTP(recorder, request)
					response := recorder.Result()
					defer response.Body.Close()
					_ = response.Write(conn)
				}(stream)
			}
		})
	}()
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				_ = authority.Refresh(ctx, machineClient)
			}
		}
	}()
	t.Cleanup(func() { cancel(); owner.Close(); <-ownerDone; <-refreshDone; authority.Close() })
	configPath := filepath.Join(filepath.Dir(ready), "native-config.json")
	configBody, _ := json.Marshal(map[string]any{"server_url": d.BaseURL, "auth": map[string]any{"profile_dir": dir, "allow_file_fallback": true}})
	if err = os.WriteFile(configPath, configBody, 0600); err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(filepath.Dir(ready), "native-ca.pem")
	if err = os.WriteFile(caPath, []byte(d.TLSCertificatePEM), 0600); err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(map[string]string{"profile_directory": dir, "issuer": d.BaseURL, "config_path": configPath, "ca_path": caPath})
	if err = os.WriteFile(ready, result, 0600); err != nil {
		t.Fatal(err)
	}
}
