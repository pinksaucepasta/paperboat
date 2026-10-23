package selfhost

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/relaylimits"

	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type EnrollRequest struct {
	Address string `json:"address"`
	Code    string `json:"code"`
	Scope   Scope  `json:"scope"`
}
type Component struct {
	Capability     string `json:"capability"`
	TCPPort        int    `json:"tcp_port"`
	QUICPort       int    `json:"quic_port"`
	Region         string `json:"region"`
	FailureDomain  string `json:"failure_domain"`
	CapacityLimit  int64  `json:"capacity_limit"`
	UsagePublicKey string `json:"usage_public_key,omitempty"`
}
type Inspection struct {
	Version                      int         `json:"version"`
	Nonce                        string      `json:"nonce"`
	InstallationKey              string      `json:"installation_key"`
	Name                         string      `json:"name"`
	EndpointHost                 string      `json:"endpoint_host"`
	InfrastructureTLSCertificate string      `json:"infrastructure_tls_certificate"`
	ExpiresAt                    int64       `json:"expires_at"`
	Components                   []Component `json:"components"`
	Signature                    string      `json:"signature,omitempty"`
}
type Registered struct {
	InstallationID    string `json:"installation_id"`
	NodeID            string `json:"node_id"`
	RuntimeCredential string `json:"runtime_credential"`
	NodeGeneration    uint64 `json:"node_generation"`
	UsageKeyID        string `json:"usage_key_id,omitempty"`
	EdgePool          string `json:"edge_pool"`
}
type RegisteredComponent struct {
	Capability string `json:"capability"`
	Registered
}
type claimRequest struct {
	ClaimID     string                `json:"claim_id"`
	ScopeType   string                `json:"scope_type"`
	ScopeID     string                `json:"scope_id"`
	ControlURL  string                `json:"control_url"`
	Components  []RegisteredComponent `json:"components"`
	JWKS        json.RawMessage       `json:"jwks"`
	Revocations json.RawMessage       `json:"revocations,omitempty"`
}
type receiptComponent struct {
	Capability     string `json:"capability"`
	InstallationID string `json:"installation_id"`
	NodeID         string `json:"node_id"`
	NodeGeneration uint64 `json:"node_generation"`
}
type receipt struct {
	ClaimID         string             `json:"claim_id"`
	ScopeType       string             `json:"scope_type"`
	ScopeID         string             `json:"scope_id"`
	InstallationKey string             `json:"installation_key"`
	Components      []receiptComponent `json:"components"`
	Signature       string             `json:"signature,omitempty"`
}

func verifySigned(key, signature string, value any) error {
	pub, err := publicKey(key)
	if err != nil {
		return err
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	b, _ := json.Marshal(value)
	if err != nil || !ed25519.Verify(pub, b, sig) {
		return ErrUnauthorized
	}
	return nil
}
func parseCode(code string) ([]byte, string, error) {
	parts := strings.Split(code, ".")
	if len(parts) != 3 || parts[0] != "pbh1" {
		return nil, "", ErrInvalid
	}
	pin, e := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	secret, e2 := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if e != nil || e2 != nil || len(pin) != 32 || len(secret) != 32 {
		return nil, "", ErrInvalid
	}
	return pin, parts[2], nil
}
func (s *Service) claimTarget(ctx context.Context, address string) (host, addressPort string, err error) {
	if address == "" || len(address) > 300 || strings.ContainsAny(address, "/@?#\\ ") {
		return "", "", ErrInvalid
	}
	host = address
	port := "8443"
	if h, p, e := net.SplitHostPort(address); e == nil {
		host, port = h, p
	} else if strings.Contains(address, ":") {
		if _, e = netip.ParseAddr(strings.Trim(address, "[]")); e != nil {
			return "", "", ErrInvalid
		}
		host = strings.Trim(address, "[]")
	}
	n, e := strconv.Atoi(port)
	if e != nil || n < 1 || n > 65535 {
		return "", "", ErrInvalid
	}
	ips, e := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if e != nil || len(ips) == 0 || len(ips) > 8 {
		return "", "", ErrUnreachable
	}
	for _, ip := range ips {
		if !s.ingressIP(ip) {
			return "", "", ErrInvalid
		}
	}
	return host, net.JoinHostPort(ips[0].String(), port), nil
}
func pinnedTransport(pin []byte, address string) *http.Transport {
	return &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, address)
	}, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return ErrUnauthorized
		}
		cert := cs.PeerCertificates[0]
		sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
		if subtle.ConstantTimeCompare(sum[:], pin) != 1 || time.Now().Before(cert.NotBefore) || !time.Now().Before(cert.NotAfter) {
			return ErrUnauthorized
		}
		for _, usage := range cert.ExtKeyUsage {
			if usage == x509.ExtKeyUsageServerAuth || usage == x509.ExtKeyUsageAny {
				return nil
			}
		}
		return ErrUnauthorized
	}}}
}

type requestNodeFailure struct {
	status int
	cause  error
}

func (*requestNodeFailure) Error() string { return "self-host endpoint request failed" }

func (e *requestNodeFailure) Unwrap() []error {
	if e.cause == nil {
		return nil
	}
	return []error{e.cause}
}

func (e *requestNodeFailure) DiagnosticStatus() int { return e.status }

var errTrailingNodeResponse = errors.New("self-host endpoint returned trailing response data")

func requestNodeStatusFailure(status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrUnauthorized
	case http.StatusGone:
		return ErrExpired
	case http.StatusConflict:
		return ErrUsed
	default:
		return &requestNodeFailure{status: status}
	}
}

func requestNode(ctx context.Context, c *http.Client, target, secret string, in, out any) (returnErr error) {
	b, e := json.Marshal(in)
	if e != nil {
		return &requestNodeFailure{cause: e}
	}
	req, e := http.NewRequestWithContext(ctx, "POST", target, bytes.NewReader(b))
	if e != nil {
		return &requestNodeFailure{cause: e}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, e := c.Do(req)
	if e != nil {
		return &requestNodeFailure{cause: e}
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			returnErr = &requestNodeFailure{status: resp.StatusCode, cause: errors.Join(returnErr, closeErr)}
		}
	}()
	if resp.StatusCode != 200 {
		return requestNodeStatusFailure(resp.StatusCode)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, (1<<20)+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return &requestNodeFailure{status: resp.StatusCode, cause: err}
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = errTrailingNodeResponse
		}
		return &requestNodeFailure{status: resp.StatusCode, cause: err}
	}
	return nil
}
func (s *Service) Enroll(ctx context.Context, actor string, in EnrollRequest, operator bool) ([]Installation, error) {
	if !s.available() || s.ControlURL == "" || s.TrustDocuments == nil {
		return nil, ErrUnauthorized
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	scope, e := s.authorizeScope(ctx, actor, in.Scope, true, operator)
	if e != nil {
		return nil, e
	}
	pin, secret, e := parseCode(in.Code)
	if e != nil {
		return nil, e
	}
	host, address, e := s.claimTarget(ctx, in.Address)
	if s.DialAddress != nil {
		address, e = s.DialAddress(ctx, in.Address)
		host = "localhost"
	}
	if e != nil {
		return nil, e
	}
	transport := pinnedTransport(pin, address)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	target := (&url.URL{Scheme: "https", Host: net.JoinHostPort(host, "8443")}).String()
	nonce := randomToken(32)
	var inspection Inspection
	if e = requestNode(ctx, client, target+"/v1/selfhost/inspect", secret, map[string]string{"nonce": nonce}, &inspection); e != nil {
		return nil, e
	}
	sig := inspection.Signature
	inspection.Signature = ""
	if e = verifySigned(inspection.InstallationKey, sig, inspection); e != nil {
		return nil, e
	}
	inspection.Signature = sig
	if inspection.Version != 1 || inspection.Nonce != nonce || !validLabel(inspection.Name, 80) || !s.validHost(inspection.EndpointHost) || len(inspection.Components) < 1 || len(inspection.Components) > 2 {
		return nil, ErrInvalid
	}
	block, _ := pem.Decode([]byte(inspection.InfrastructureTLSCertificate))
	if block == nil {
		return nil, ErrInvalid
	}
	cert, e := x509.ParseCertificate(block.Bytes)
	if e != nil {
		return nil, ErrInvalid
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	if subtle.ConstantTimeCompare(sum[:], pin) != 1 || cert.VerifyHostname(inspection.EndpointHost) != nil {
		return nil, ErrUnauthorized
	}
	seen := map[string]bool{}
	for _, c := range inspection.Components {
		if seen[c.Capability] || !validComponent(c) {
			return nil, ErrInvalid
		}
		seen[c.Capability] = true
		if c.Capability == "tunnel" {
			if _, e = publicKey(c.UsagePublicKey); e != nil || c.UsagePublicKey == inspection.InstallationKey {
				return nil, ErrInvalid
			}
		}
	}
	components, claimID, e := s.reserve(ctx, actor, scope, inspection, pin)
	if e != nil {
		return nil, e
	}
	jwks, revocations, e := s.TrustDocuments(ctx)
	if e != nil {
		return nil, e
	}
	claim := claimRequest{ClaimID: claimID, ScopeType: scope.Kind, ScopeID: scope.ID, ControlURL: s.ControlURL, Components: components, JWKS: jwks, Revocations: revocations}
	var accepted receipt
	if e = requestNode(ctx, client, target+"/v1/selfhost/claim", secret, claim, &accepted); e != nil {
		return nil, e
	}
	sig = accepted.Signature
	accepted.Signature = ""
	if accepted.ClaimID != claimID || accepted.ScopeType != scope.Kind || accepted.ScopeID != scope.ID || accepted.InstallationKey != inspection.InstallationKey || len(accepted.Components) != len(components) {
		return nil, ErrUnauthorized
	}
	if e = verifySigned(accepted.InstallationKey, sig, accepted); e != nil {
		return nil, e
	}
	for idx, c := range components {
		r := accepted.Components[idx]
		if r.Capability != c.Capability || r.InstallationID != c.InstallationID || r.NodeID != c.NodeID || r.NodeGeneration != c.NodeGeneration {
			return nil, ErrUnauthorized
		}
	}
	if _, e = s.database.SQL().ExecContext(ctx, `UPDATE paperboat.selfhost_installations SET claim_complete=true WHERE claim_id=$1 AND scope_kind=$2 AND scope_id=$3 AND state='active'`, claimID, scope.Kind, scope.ID); e != nil {
		return nil, e
	}
	return s.ListScoped(ctx, actor, scope, operator)
}
func (s *Service) reserve(ctx context.Context, actor string, scope Scope, i Inspection, pin []byte) (registered []RegisteredComponent, resultClaimID string, returnErr error) {
	tx, e := s.database.SQL().BeginTx(ctx, nil)
	if e != nil {
		return nil, "", e
	}
	defer func() { returnErr = withTransactionRollbackFailure("reserve_enrollment", returnErr, tx.Rollback()) }()
	key, _ := publicKey(i.InstallationKey)
	if _, e = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, i.InstallationKey); e != nil {
		return nil, "", e
	}
	if e = lockTeamWrite(ctx, tx, actor, scope); e != nil {
		return nil, "", e
	}
	if _, e = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,1))`, scope.Kind+":"+scope.ID); e != nil {
		return nil, "", e
	}
	var existing int
	if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM paperboat.selfhost_installations WHERE installation_public_key=$1`, []byte(key)).Scan(&existing); e != nil {
		return nil, "", e
	}
	if existing != 0 && existing != len(i.Components) {
		return nil, "", ErrUsed
	}
	var total, scoped int
	if e = tx.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER(WHERE scope_kind=$1 AND scope_id=$2) FROM paperboat.selfhost_installations WHERE state='active'`, scope.Kind, scope.ID).Scan(&total, &scoped); e != nil {
		return nil, "", e
	}
	if existing == 0 && (total+len(i.Components) > 16384 || scoped+len(i.Components) > 256) {
		return nil, "", ErrLimit
	}
	claimID := "claim_" + randomIdentity()
	out := []RegisteredComponent{}
	for _, c := range i.Components {
		var r Registered
		var kind, id, state, oldClaim string
		var ciphertext []byte
		e = tx.QueryRowContext(ctx, `SELECT id,node_id,runtime_ciphertext,state,scope_kind,scope_id,claim_id FROM paperboat.selfhost_installations WHERE installation_public_key=$1 AND capability=$2 FOR UPDATE`, []byte(key), c.Capability).Scan(&r.InstallationID, &r.NodeID, &ciphertext, &state, &kind, &id, &oldClaim)
		if e == nil {
			if state != "active" {
				return nil, "", ErrRevoked
			}
			if kind != scope.Kind || id != scope.ID {
				return nil, "", ErrUsed
			}
			claimID = oldClaim
			r.RuntimeCredential, e = secrets.Decrypt(s.key, ciphertext)
			if e != nil {
				return nil, "", e
			}
			if e = tx.QueryRowContext(ctx, `SELECT 1 FROM paperboat.control_tunnel_nodes WHERE id=$1 AND tls_spki_sha256=$2 AND endpoint_host=$3 AND endpoint_tcp_port=$4 AND endpoint_quic_port=$5`, r.NodeID, pin, i.EndpointHost, c.TCPPort, c.QUICPort).Scan(&r.NodeGeneration); e != nil {
				return nil, "", ErrUsed
			}
			if c.Capability == "tunnel" {
				if e = tx.QueryRowContext(ctx, `SELECT key_id FROM paperboat.control_usage_verification_keys WHERE edge_node_id=$1 AND public_key=$2 AND revoked_at IS NULL LIMIT 1`, r.NodeID, mustPublicKey(c.UsagePublicKey)).Scan(&r.UsageKeyID); e != nil {
					return nil, "", e
				}
			}
			r.EdgePool = r.InstallationID
			out = append(out, RegisteredComponent{c.Capability, r})
			continue
		}
		if e != sql.ErrNoRows {
			return nil, "", e
		}
		if !time.Unix(i.ExpiresAt, 0).After(s.now()) {
			return nil, "", ErrExpired
		}
		r = Registered{InstallationID: "installation_" + randomIdentity(), NodeID: "node_" + randomIdentity(), RuntimeCredential: randomToken(32), NodeGeneration: 1}
		ciphertext, e = secrets.Encrypt(s.key, r.RuntimeCredential)
		if e != nil {
			return nil, "", e
		}
		roles, transports := `["relay"]`, `["derp_quic","derp_wss"]`
		if c.Capability == "tunnel" {
			roles, transports = `["edge"]`, `["http3","http2"]`
		}
		now := s.now().UTC()
		_, e = tx.ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,allowed_account_ids,tls_spki_sha256) VALUES($1,$2,'1.0',$3,$4,$5,$6,'registered',false,$7,$8,ARRAY(SELECT jsonb_array_elements_text($9::jsonb)),ARRAY(SELECT jsonb_array_elements_text($10::jsonb)),$11,0,$12,$12,NULL,$13)`, r.NodeID, r.InstallationID, "", i.EndpointHost, c.TCPPort, c.QUICPort, c.Region, c.FailureDomain, roles, transports, c.CapacityLimit, now, pin)
		if e != nil {
			return nil, "", e
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, r.InstallationID, r.NodeID, []byte(key), digest(r.RuntimeCredential), ciphertext, i.Name, c.Capability, scope.Kind, scope.ID, claimID)
		if e != nil {
			return nil, "", e
		}
		if c.Capability == "tunnel" {
			usage, _ := publicKey(c.UsagePublicKey)
			r.UsageKeyID = "shu_" + r.NodeID
			_, e = tx.ExecContext(ctx, `INSERT INTO paperboat.control_usage_verification_keys(key_id,edge_node_id,public_key,not_before,expires_at) VALUES($1,$2,$3,$4,$5)`, r.UsageKeyID, r.NodeID, []byte(usage), now.Add(-time.Minute), now.Add(365*24*time.Hour))
			if e != nil {
				return nil, "", e
			}
		}
		r.EdgePool = r.InstallationID
		out = append(out, RegisteredComponent{c.Capability, r})
	}
	if e = tx.Commit(); e != nil {
		return nil, "", e
	}
	return out, claimID, nil
}

func mustPublicKey(value string) []byte { key, _ := publicKey(value); return []byte(key) }

// validComponent bounds one advertised component. A relay's capacity is bounded by the ceiling the
// relay itself enforces; a tunnel keeps its own bound.
func validComponent(c Component) bool {
	ceiling := map[string]int64{"relay": relaylimits.MaxConnectionsPerNode, "tunnel": 256}[c.Capability]
	switch {
	case ceiling == 0 || c.CapacityLimit < 1 || c.CapacityLimit > ceiling:
		return false
	case c.TCPPort < 1 || c.TCPPort > 65535 || c.QUICPort < 1 || c.QUICPort > 65535:
		return false
	case c.Capability == "tunnel" && c.TCPPort == c.QUICPort:
		return false
	}
	return validRegion(c.Region) && validLabel(c.FailureDomain, 128)
}
