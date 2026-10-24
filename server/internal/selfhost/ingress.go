package selfhost

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/nodeauth"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

var ErrIngress = errors.New("public ingress verification failed; check the installation hostname A/AAAA records, HTTPS port 443, certificate and running edge, then retry verify-ingress")

type ingressProbeFailure struct{ cause error }

func (e *ingressProbeFailure) Error() string        { return ErrIngress.Error() }
func (e *ingressProbeFailure) Unwrap() error        { return e.cause }
func (e *ingressProbeFailure) Is(target error) bool { return target == ErrIngress }

// IsIngressProbeFailure recognizes only the direct producer-owned expected
// probe outcome. A joined independent database or cleanup failure is not a
// retryable ingress conflict.
func IsIngressProbeFailure(err error) bool {
	failure, ok := err.(*ingressProbeFailure)
	return ok && failure != nil
}

func ingressProbeError(err error) error {
	if err == nil || errors.Is(err, ErrIngress) {
		return err
	}
	return &ingressProbeFailure{cause: err}
}

func publicIngressIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32"} {
		if netip.MustParsePrefix(prefix).Contains(ip) {
			return false
		}
	}
	return ip.Is4() || netip.MustParsePrefix("2000::/3").Contains(ip)
}

// VerifyIngress probes only the installation's registered public hostname on
// HTTPS 443. Resolution is pinned before dialing, redirects/proxies are disabled,
// and every advertised address must prove possession of this node's credential.
func (s *Service) VerifyIngress(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	var node, host, epoch string
	if err := s.database.SQL().QueryRowContext(ctx, `SELECT n.id,n.endpoint_host,n.process_epoch FROM paperboat.selfhost_installations i JOIN paperboat.control_tunnel_nodes n ON n.id=i.node_id WHERE i.id=$1 AND i.state='active' AND i.capability='tunnel' AND (i.expires_at IS NULL OR i.expires_at>now()) AND `+nodeLiveSQL, id).Scan(&node, &host, &epoch); err != nil {
		if err == sql.ErrNoRows {
			return ErrIngress
		}
		return err
	}
	credential, err := s.RuntimeCredential(ctx, node)
	if err != nil {
		if err == nodeauth.ErrNotFound {
			return ErrIngress
		}
		return err
	}
	v4, v6, err := probeIngress(ctx, host, node, credential, s.ingressIP)
	if err != nil {
		return err
	}
	result, err := s.database.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes n SET public_ingress_ipv4=$3::inet,public_ingress_ipv6=$4::inet,public_ingress_verified_at=now() WHERE n.id=$1 AND n.process_epoch=$2 AND n.endpoint_host=$5 AND EXISTS(SELECT 1 FROM paperboat.selfhost_installations i WHERE i.id=$6 AND i.node_id=n.id AND i.state='active' AND i.runtime_hash=$7)`, node, epoch, v4, v6, host, id, digest(credential))
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrIngress
	}
	return nil
}

// ProbePublicIngress verifies that every public address for host serves this
// exact node's credential over publicly trusted HTTPS, independently of its
// pinned carrier identity. The caller must fence its state write.
func ProbePublicIngress(ctx context.Context, host, node, credential string) (any, any, error) {
	return probeIngress(ctx, host, node, credential, publicIngressIP)
}

// probeIngress is ProbePublicIngress with the address rule given: managed nodes stay public-only, and a
// self-hosted node may be private when the service allows it (staging).
func probeIngress(ctx context.Context, host, node, credential string, allowed func(netip.Addr) bool) (any, any, error) {
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, nil, ingressProbeError(err)
	}
	if len(addresses) == 0 || len(addresses) > 2 {
		return nil, nil, ErrIngress
	}
	var v4, v6 any
	seen := map[netip.Addr]bool{}
	for _, ip := range addresses {
		ip = ip.Unmap()
		if !allowed(ip) || seen[ip] {
			return nil, nil, ErrIngress
		}
		seen[ip] = true
		if ip.Is4() {
			if v4 != nil {
				return nil, nil, ErrIngress
			}
			v4 = ip.String()
		} else {
			if v6 != nil {
				return nil, nil, ErrIngress
			}
			v6 = ip.String()
		}
	}
	var nonce [32]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return nil, nil, err
	}
	challenge := base64.RawURLEncoding.EncodeToString(nonce[:])
	for _, ip := range addresses {
		if err = verifyIngressAddress(ctx, host, ip, node, challenge, credential); err != nil {
			return nil, nil, ingressProbeError(err)
		}
	}
	return v4, v6, nil
}

func verifyIngressAddress(ctx context.Context, host string, ip netip.Addr, node, challenge, credential string) (returnErr error) {
	transport := publicIngressTransport(host, ip)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, "GET", "https://"+host+"/.well-known/paperboat-installation?challenge="+challenge, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, resp.Body.Close())
	}()
	if resp.StatusCode != 200 {
		return ErrIngress
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1025))
	if err != nil {
		return err
	}
	if len(raw) > 1024 {
		return ErrIngress
	}
	var out struct {
		NodeID    string `json:"node_id"`
		Challenge string `json:"challenge"`
		Proof     string `json:"proof"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	if out.NodeID != node || out.Challenge != challenge {
		return ErrIngress
	}
	proof, err := base64.RawURLEncoding.DecodeString(out.Proof)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, []byte(credential))
	mac.Write([]byte("paperboat-installation/v1\n" + node + "\n" + challenge))
	if !hmac.Equal(proof, mac.Sum(nil)) {
		return ErrIngress
	}
	return nil
}

func (s *Service) VerifyIngressScoped(ctx context.Context, actor, id string, operator bool) error {
	var scope Scope
	if err := s.database.SQL().QueryRowContext(ctx, `SELECT scope_kind,scope_id FROM paperboat.selfhost_installations WHERE id=$1 AND state='active'`, id).Scan(&scope.Kind, &scope.ID); err != nil {
		if err == sql.ErrNoRows {
			return ErrUnauthorized
		}
		return err
	}
	if _, err := s.authorizeScope(ctx, actor, scope, true, operator); err != nil {
		return err
	}
	return s.VerifyIngress(ctx, id)
}

// ReconcileIngress probes only claimed, authenticated nodes; it never discovers installations.
func (s *Service) ReconcileIngress(ctx context.Context) (returnErr error) {
	s.ingressMu.Lock()
	var observedErr error
	defer func() {
		if observedErr == nil {
			observedErr = returnErr
		}
		s.ingressMu.Unlock()
		s.observeIngressIteration(ctx, observedErr)
	}()
	rows, err := s.database.SQL().QueryContext(ctx, `SELECT i.id FROM paperboat.selfhost_installations i JOIN paperboat.control_tunnel_nodes n ON n.id=i.node_id WHERE i.state='active' AND i.claim_complete AND i.capability='tunnel' AND n.state='ready' AND n.ready AND n.public_ingress_verified_at IS NULL AND i.id>$1 ORDER BY i.id LIMIT 8`, s.ingressCursor)
	if err != nil {
		returnErr = err
		return returnErr
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			returnErr = withDatabaseCleanupFailure("reconcile_ingress", err, rows.Close())
			return returnErr
		}
		ids = append(ids, id)
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	err = withDatabaseCleanupFailure("reconcile_ingress", rowsErr, closeErr)
	if err != nil {
		returnErr = err
		return returnErr
	}
	if len(ids) == 0 {
		s.ingressCursor = ""
		return nil
	}
	s.ingressCursor = ids[len(ids)-1]
	probe := s.VerifyIngress
	if s.ProbeIngress != nil {
		probe = s.ProbeIngress
	}
	var probes sync.WaitGroup
	var firstProbeErr error
	var firstProbeErrMu sync.Mutex
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		probes.Go(func() {
			if err := probe(ctx, id); err != nil {
				fault := supportreport.ProjectFault(ctx, "paperboat-server", "worker_edge_dns", "reconciliation", "edge_dns_reconcile_failed", err)
				if fault.Outcome == "canceled" {
					return
				}
				firstProbeErrMu.Lock()
				if firstProbeErr == nil {
					firstProbeErr = err
				}
				firstProbeErrMu.Unlock()
			}
		})
	}
	probes.Wait()
	observedErr = firstProbeErr
	returnErr = ctx.Err()
	if observedErr != nil && returnErr != nil {
		observedErr = errors.Join(observedErr, returnErr)
	}
	return returnErr
}

func (s *Service) observeIngressIteration(ctx context.Context, err error) {
	const component, operation, stage, code = "paperboat-server", "worker_edge_dns", "reconciliation", "edge_dns_reconcile_failed"
	if err != nil {
		fault := supportreport.ProjectFault(ctx, component, operation, stage, code, err)
		if fault.Outcome == "canceled" {
			return
		}
		class := strings.Join([]string{fault.Cause, fault.ErrorType, strings.Join(fault.ErrorChain, ","), strconv.Itoa(fault.Errno)}, "\x00")
		s.ingressObservationMu.Lock()
		if class == s.ingressFailureClass {
			s.ingressObservationMu.Unlock()
			return
		}
		reference := s.ingressFailureReference
		if reference == "" {
			reference = supportreport.NewReference()
			s.ingressFailureReference = reference
		}
		s.ingressFailureClass = class
		s.ingressObservationMu.Unlock()
		faultCtx := supportreport.WithFailureIdentity(ctx, "", "", reference)
		supportreport.ObserveFromContext(faultCtx, component, operation, stage, code, err)
		return
	}
	s.ingressObservationMu.Lock()
	reference := s.ingressFailureReference
	s.ingressFailureReference, s.ingressFailureClass = "", ""
	s.ingressObservationMu.Unlock()
	if reference != "" {
		faultCtx := supportreport.WithFailureIdentity(ctx, "", "", reference)
		supportreport.CaptureFromContext(faultCtx, component, operation, "worker_recovery", "worker_recovered", nil)
	}
}
func (s *Service) Run(ctx context.Context) {
	timer := time.NewTicker(15 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			request, cancel := context.WithTimeout(ctx, 15*time.Second)
			_ = s.ReconcileIngress(request)
			cancel()
		}
	}
}

func publicIngressTransport(host string, ip netip.Addr) *http.Transport {
	dialer := &net.Dialer{Timeout: 4 * time.Second}
	return &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), "443"))
	}, TLSHandshakeTimeout: 4 * time.Second, ResponseHeaderTimeout: 4 * time.Second, DisableKeepAlives: true, MaxResponseHeaderBytes: 4096}
}
