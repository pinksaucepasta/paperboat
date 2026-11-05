package tunnelcert

// This file contains the internal server-to-edge certificate distribution
// transport.  It is deliberately separate from the public HTTP API: the
// endpoint is mounted only beneath the authenticated edge-control handler and
// carries certificate bytes over that already authenticated TLS connection.
// No message is written to an audit record or log, and the hub drops the
// plaintext bundle as soon as the edge acknowledges the action.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	serverdiagnostics "github.com/pinksaucepasta/paperboat/server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/server/internal/nodeauth"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

const (
	CertificateDistributionPullPath    = "/v1/edge/certificates/distributions/pull"
	CertificateDistributionAckPath     = "/v1/edge/certificates/distributions/ack"
	CertificateDistributionRequestPath = "/v1/edge/certificates/distributions/request"
	maxDistributionBody                = 16 << 20
	maxDistributionMessages            = 128
	maxDistributionLifetime            = 10 * time.Minute
)

var (
	ErrDistributionTransportInvalid = errors.New("invalid certificate distribution transport")
	ErrDistributionTransportAuth    = errors.New("certificate distribution transport is not authenticated")
	ErrDistributionTransportStale   = errors.New("certificate distribution message is stale")
	ErrDistributionTransportFailed  = errors.New("certificate distribution was rejected")
)

// DistributionMessage is an internal wire message. CertificatePEM and
// PrivateKeyPEM are only present on this authenticated control transport;
// callers must never marshal this type into a public response, audit event,
// or log line.
type DistributionMessage struct {
	Version               int       `json:"version"`
	Action                string    `json:"action"`
	CertificateID         string    `json:"certificate_id"`
	AccountID             string    `json:"account_id"`
	TunnelID              string    `json:"tunnel_id"`
	DomainID              string    `json:"domain_id"`
	TargetKind            string    `json:"target_kind"`
	RouteID               string    `json:"route_id"`
	PreviewID             string    `json:"preview_id"`
	PreviewGeneration     uint64    `json:"preview_generation"`
	PreviewState          string    `json:"preview_state"`
	PreviewExpiresAt      time.Time `json:"preview_expires_at"`
	Hostname              string    `json:"hostname"`
	OnDemandLeaf          bool      `json:"on_demand_leaf"`
	DomainGeneration      uint64    `json:"domain_generation"`
	CertificateGeneration uint64    `json:"certificate_generation"`
	EdgeNodeID            string    `json:"edge_node_id"`
	EdgeProcessEpoch      string    `json:"edge_process_epoch"`
	AssignmentGeneration  uint64    `json:"assignment_generation"`
	Fingerprint           string    `json:"fingerprint"`
	Issuer                string    `json:"issuer"`
	NotBefore             time.Time `json:"not_before"`
	// ExpiresAt is the short-lived transport envelope deadline.  It is
	// deliberately independent from the certificate's validity interval so a
	// queued message can never be replayed for the lifetime of a leaf.
	ExpiresAt            time.Time `json:"expires_at"`
	CertificateExpiresAt time.Time `json:"certificate_expires_at"`
	CertificatePEM       []byte    `json:"certificate_pem"`
	PrivateKeyPEM        []byte    `json:"private_key_pem"`
	IssuedAt             time.Time `json:"issued_at"`
	Proof                string    `json:"proof"`
}

type distributionAck struct {
	Version               int       `json:"version"`
	Action                string    `json:"action"`
	CertificateID         string    `json:"certificate_id"`
	AccountID             string    `json:"account_id"`
	TunnelID              string    `json:"tunnel_id"`
	DomainID              string    `json:"domain_id"`
	TargetKind            string    `json:"target_kind"`
	RouteID               string    `json:"route_id"`
	PreviewID             string    `json:"preview_id"`
	PreviewGeneration     uint64    `json:"preview_generation"`
	PreviewState          string    `json:"preview_state"`
	PreviewExpiresAt      time.Time `json:"preview_expires_at"`
	Hostname              string    `json:"hostname"`
	DomainGeneration      uint64    `json:"domain_generation"`
	CertificateGeneration uint64    `json:"certificate_generation"`
	EdgeNodeID            string    `json:"edge_node_id"`
	EdgeProcessEpoch      string    `json:"edge_process_epoch"`
	AssignmentGeneration  uint64    `json:"assignment_generation"`
	Fingerprint           string    `json:"fingerprint"`
	Status                string    `json:"status"`
	Code                  string    `json:"code,omitempty"`
}

type distributionPull struct {
	EdgeNodeID       string `json:"edge_node_id"`
	EdgeProcessEpoch string `json:"edge_process_epoch"`
	Limit            int    `json:"limit,omitempty"`
	Cursor           string `json:"cursor,omitempty"`
}

// distributionPage is the pull response. The edge treats a complete page with
// no messages as nothing to install.
type distributionPage struct {
	Version    int                   `json:"version"`
	Complete   bool                  `json:"complete"`
	NextCursor string                `json:"next_cursor,omitempty"`
	Messages   []DistributionMessage `json:"messages"`
}

type distributionRequest struct {
	Hostname string `json:"hostname"`
}

// DistributionHub is both the CertificateDistributor transport used by the
// server coordinator and the authenticated pull/ack endpoint consumed by an
// edge.  A pending action is retained until the exact node, process epoch,
// certificate, assignment generation, and fingerprint are acknowledged.
// This makes retries idempotent while fencing stale edge processes.
type DistributionHub struct {
	nodeCredentials nodeauth.Resolver
	credential      []byte
	credentialMu    sync.RWMutex
	now             func() time.Time
	maximum         int
	identity        DistributionNodeIdentityResolver
	onDemand        func(context.Context, DistributionNodeIdentity, string) error
	store           DistributionActionStore
	keys            MasterKeySource
	keyReference    string

	mu       sync.Mutex
	closed   bool
	closedCh chan struct{} // closed by Close, wakes waiters
}

type DistributionHubConfig struct {
	NodeCredentials nodeauth.Resolver
	Credential      []byte
	MaximumPending  int
	// Store holds pending actions and results for every server instance; Keys seals a staged bundle in it.
	Store        DistributionActionStore
	Keys         MasterKeySource
	KeyReference string
	Now          func() time.Time
	// Identity authenticates the edge process independently of the shared
	// control bearer.  Pull/ACK bodies are compared to this result and are
	// never used as an identity source.
	Identity DistributionNodeIdentityResolver
	// OnDemand handles an authenticated exact-SNI request. The callback must
	// resolve verified wildcard policy and the current edge assignment from
	// server state before issuing or distributing a leaf.
	OnDemand func(context.Context, DistributionNodeIdentity, string) error
}

// DistributionNodeIdentity is the authenticated edge-control principal.
// Implementations should derive it from the mTLS peer certificate or an
// equivalent node-specific proof, not from a request body/header.
type DistributionNodeIdentity struct {
	NodeID       string
	ProcessEpoch string
}

type DistributionNodeIdentityResolver interface {
	ResolveDistributionNode(context.Context, *http.Request) (DistributionNodeIdentity, error)
}

type DistributionNodeIdentityResolverFunc func(context.Context, *http.Request) (DistributionNodeIdentity, error)

func (f DistributionNodeIdentityResolverFunc) ResolveDistributionNode(ctx context.Context, r *http.Request) (DistributionNodeIdentity, error) {
	return f(ctx, r)
}

func NewDistributionHub(config DistributionHubConfig) (*DistributionHub, error) {
	if !validDistributionCredential(config.Credential) {
		return nil, fmt.Errorf("%w: credential is required", ErrDistributionTransportInvalid)
	}
	if config.MaximumPending <= 0 || config.MaximumPending > 4096 {
		config.MaximumPending = 1024
	}
	if config.Store == nil || config.Keys == nil || !validKeyReference(config.KeyReference) {
		return nil, fmt.Errorf("%w: action store and master keys are required", ErrDistributionTransportInvalid)
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	if config.Identity == nil {
		return nil, fmt.Errorf("%w: node identity resolver is required", ErrDistributionTransportInvalid)
	}
	return &DistributionHub{nodeCredentials: config.NodeCredentials, credential: append([]byte(nil), config.Credential...), now: config.Now, maximum: config.MaximumPending, identity: config.Identity, onDemand: config.OnDemand, store: config.Store, keys: config.Keys, keyReference: config.KeyReference, closedCh: make(chan struct{})}, nil
}

// Close invalidates all outstanding distribution actions and wipes every
// encoded certificate/private-key buffer retained by the hub. A closed hub
// cannot be restarted; callers should construct a new deployment component
// for a new process epoch.
func (h *DistributionHub) Close() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	if h.closedCh != nil {
		close(h.closedCh)
	}
	h.credentialMu.Lock()
	clear(h.credential)
	h.credential = nil
	h.credentialMu.Unlock()
	h.mu.Unlock()
	return nil
}

// Handler returns an internal handler.  It authenticates independently so a
// caller cannot accidentally mount it outside EdgeService's authenticated
// mux without preserving the machine credential boundary.
func (h *DistributionHub) Handler() http.Handler {
	if h == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+CertificateDistributionPullPath, h.handlePull)
	mux.HandleFunc("POST "+CertificateDistributionAckPath, h.handleAck)
	mux.HandleFunc("POST "+CertificateDistributionRequestPath, h.handleRequest)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorized, authErr := h.authorized(r)
		if authErr != nil {
			if clientClosed(r, authErr) {
				w.WriteHeader(statusClientClosedRequest)
				return
			}
			captureDistributionAuthorizationFailure(r, "control_unavailable", authErr)
			writeDistributionError(w, http.StatusServiceUnavailable)
			return
		}
		if !authorized {
			writeDistributionError(w, http.StatusUnauthorized)
			return
		}
		identity, err := h.identity.ResolveDistributionNode(r.Context(), r)
		if err != nil {
			if pureDistributionError(err, ErrDistributionIdentityInvalid) {
				writeDistributionError(w, http.StatusUnauthorized)
			} else if clientClosed(r, err) {
				w.WriteHeader(statusClientClosedRequest)
			} else {
				captureDistributionAuthorizationFailure(r, "control_unavailable", err)
				writeDistributionError(w, http.StatusServiceUnavailable)
			}
			return
		}
		if !validDistributionIdentity(identity.NodeID, identity.ProcessEpoch) {
			writeDistributionError(w, http.StatusUnauthorized)
			return
		}
		r = r.WithContext(withDistributionNodeIdentity(r.Context(), identity))
		mux.ServeHTTP(w, r)
	})
}

// Stage queues a certificate for the authenticated edge transport.
func (h *DistributionHub) Stage(ctx context.Context, request DistributionRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	_, err := h.enqueue(ctx, "stage", request)
	return err
}

func (h *DistributionHub) WaitReady(ctx context.Context, request DistributionRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	return h.wait(ctx, distributionKey("stage", request))
}

func (h *DistributionHub) Activate(ctx context.Context, request DistributionRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	return h.waitForAction(ctx, "activate", request, "active")
}

func (h *DistributionHub) Retire(ctx context.Context, certificate StoredCertificate, target DistributionTarget) error {
	if err := certificate.validateDistributionMetadata(); err != nil {
		return err
	}
	if err := target.Validate(); err != nil {
		return err
	}
	return h.waitForAction(ctx, "retire", DistributionRequest{Certificate: certificate, Target: target}, "retired")
}

// Revoke is used by SQLDistributor when a durable certificate is revoked.
// It differs from Retire so the edge registry records the terminal revoked
// state rather than merely retiring a still-authoritative certificate.
func (h *DistributionHub) Revoke(ctx context.Context, certificate StoredCertificate, target DistributionTarget) error {
	if err := certificate.validateDistributionMetadata(); err != nil {
		return err
	}
	if err := target.Validate(); err != nil {
		return err
	}
	return h.waitForAction(ctx, "revoke", DistributionRequest{Certificate: certificate, Target: target}, "revoked")
}

func (h *DistributionHub) waitForAction(ctx context.Context, action string, request DistributionRequest, status string) error {
	if _, err := h.enqueue(ctx, action, request); err != nil {
		return err
	}
	return h.waitStatus(ctx, distributionKey(action, request), status)
}

func (h *DistributionHub) enqueue(ctx context.Context, action string, request DistributionRequest) (DistributionMessage, error) {
	if h == nil {
		return DistributionMessage{}, ErrDistributionTransportInvalid
	}
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		return DistributionMessage{}, ErrDistributionTransportInvalid
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := distributionContextError(ctx); err != nil {
		return DistributionMessage{}, err
	}
	if action != "stage" && action != "activate" && action != "retire" && action != "revoke" {
		return DistributionMessage{}, ErrDistributionTransportInvalid
	}
	now := h.now().UTC()
	message := DistributionMessage{
		Version: 1, Action: action, CertificateID: request.Certificate.ID,
		AccountID: request.Certificate.AccountID, TunnelID: request.Certificate.TunnelID,
		DomainID: request.Certificate.DomainID, TargetKind: string(request.Certificate.TargetKind), RouteID: request.Certificate.RouteID,
		PreviewID: request.Certificate.PreviewID, PreviewGeneration: request.Certificate.PreviewGeneration, PreviewState: request.Certificate.PreviewState, PreviewExpiresAt: request.Certificate.PreviewExpiresAt,
		Hostname:         request.Certificate.Hostname,
		OnDemandLeaf:     request.Certificate.LeafHostname == "" && request.Certificate.Strategy == StrategyOnDemandLeaf,
		DomainGeneration: request.Certificate.DomainGeneration, CertificateGeneration: request.Certificate.CertificateGeneration,
		EdgeNodeID: request.Target.NodeID, EdgeProcessEpoch: request.Target.ProcessEpoch,
		AssignmentGeneration: request.Target.Generation, Fingerprint: base64.RawURLEncoding.EncodeToString(request.Certificate.Fingerprint[:]),
		Issuer: request.Certificate.Issuer, NotBefore: request.Certificate.NotBefore,
		ExpiresAt: distributionExpiry(now, request.Certificate.ExpiresAt), CertificateExpiresAt: request.Certificate.ExpiresAt,
		CertificatePEM: append([]byte(nil), request.Bundle.CertificatePEM...), PrivateKeyPEM: append([]byte(nil), request.Bundle.PrivateKeyPEM...), IssuedAt: now,
	}
	if action != "stage" {
		// The edge needs the exact previously staged bundle metadata for every
		// action, but sending bytes again is unnecessary.  Keep action messages
		// secret-minimal and let the receiver use its staged in-memory entry.
		message.CertificatePEM = nil
		message.PrivateKeyPEM = nil
	}
	var signErr error
	message.Proof, signErr = h.sign(ctx, message)
	if signErr != nil {
		clear(message.PrivateKeyPEM)
		return DistributionMessage{}, signErr
	}
	if action == "stage" && !distributionMessageFitsResponse(message) {
		clear(message.PrivateKeyPEM)
		return DistributionMessage{}, ErrDistributionTransportFailed
	}
	key := distributionStoreKey(distributionMessageKey(message))
	if err := h.store.Purge(ctx, now); err != nil {
		return DistributionMessage{}, distributionOperationFailure{cause: err}
	}
	if action != "stage" {
		// Activate/retire/revoke are meaningful only for the exact staged certificate target. Never queue an
		// action an edge could accept without a corresponding stage record after a restart or ledger expiry.
		stage, staged, err := h.store.Get(ctx, distributionStoreKey(distributionKey("stage", request)))
		if err != nil {
			return DistributionMessage{}, distributionOperationFailure{cause: err}
		}
		if !staged || stage.Status != "" && stage.Status != "ready" && stage.Status != "active" {
			return DistributionMessage{}, ErrDistributionTransportStale
		}
	}
	if existing, found, err := h.store.Get(ctx, key); err != nil {
		return DistributionMessage{}, distributionOperationFailure{cause: err}
	} else if found && existing.Status != "failed" && (existing.Status != "" || existing.Message.ExpiresAt.After(now)) {
		clear(message.PrivateKeyPEM)
		return existing.Message, nil
	}
	if pending, err := h.store.CountPending(ctx, now); err != nil {
		return DistributionMessage{}, distributionOperationFailure{cause: err}
	} else if pending >= h.maximum {
		return DistributionMessage{}, ErrDistributionTransportFailed
	}
	row := DistributionAction{Key: key, Message: message}
	if action == "stage" && (len(message.CertificatePEM) > 0 || len(message.PrivateKeyPEM) > 0) {
		sealedCertificate, sealedKey, err := SealParts(ctx, h.keys, h.keyReference, CertificateBundle{CertificatePEM: message.CertificatePEM, PrivateKeyPEM: message.PrivateKeyPEM, Issuer: request.Certificate.Issuer, NotBefore: request.Certificate.NotBefore, NotAfter: request.Certificate.ExpiresAt})
		if err != nil {
			wipeDistributionMessage(&message)
			return DistributionMessage{}, distributionOperationFailure{cause: err}
		}
		row.MasterKeyReference, row.SealedCertificate, row.SealedPrivateKey = h.keyReference, sealedCertificate, sealedKey
	}
	row.Message.CertificatePEM, row.Message.PrivateKeyPEM = nil, nil
	if _, err := h.store.Put(ctx, row); err != nil {
		wipeDistributionMessage(&message)
		return DistributionMessage{}, distributionOperationFailure{cause: err}
	}
	return message, nil
}

func (h *DistributionHub) wait(ctx context.Context, key string) error {
	return h.waitStatus(ctx, key, "ready")
}

// waitStatus polls the shared store until an edge answers on any instance, the action expires or ctx ends.
func (h *DistributionHub) waitStatus(ctx context.Context, key, want string) error {
	if h == nil {
		return ErrDistributionTransportInvalid
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key = distributionStoreKey(key)
	delay := 50 * time.Millisecond
	for {
		action, found, err := h.store.Get(ctx, key)
		switch {
		case err != nil:
			return distributionOperationFailure{cause: err}
		case !found:
			return ErrDistributionTransportStale
		case action.Status == want:
			return nil
		case action.Status == "failed":
			return fmt.Errorf("%w: %s", ErrDistributionTransportFailed, action.Code)
		case action.Status != "":
			return ErrDistributionTransportStale
		case !action.Message.ExpiresAt.After(h.now().UTC()):
			return ErrDistributionTransportStale
		}
		timer := time.NewTimer(min(delay, action.Message.ExpiresAt.Sub(h.now().UTC())))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-h.closedCh:
			timer.Stop()
			return ErrDistributionTransportStale
		case <-timer.C:
		}
		delay = min(2*delay, time.Second)
	}
}

func (h *DistributionHub) handlePull(w http.ResponseWriter, r *http.Request) {
	var input distributionPull
	if !decodeDistributionBody(w, r, &input) || !validDistributionIdentity(input.EdgeNodeID, input.EdgeProcessEpoch) {
		return
	}
	if !validDistributionCursor(input.Cursor) {
		writeDistributionError(w, http.StatusBadRequest)
		return
	}
	cursor, ok := decodeDistributionCursor(input.Cursor)
	if !ok {
		writeDistributionError(w, http.StatusBadRequest)
		return
	}
	identity, ok := distributionNodeIdentityFromContext(r.Context())
	if !ok || input.EdgeNodeID != identity.NodeID || input.EdgeProcessEpoch != identity.ProcessEpoch {
		writeDistributionError(w, http.StatusForbidden)
		return
	}
	limit := input.Limit
	if limit <= 0 || limit > maxDistributionMessages {
		limit = maxDistributionMessages
	}
	now := h.now().UTC()
	// One row past the limit tells whether the page is complete.
	actions, err := h.store.Pending(r.Context(), input.EdgeNodeID, input.EdgeProcessEpoch, cursor, limit+1, now)
	if err != nil {
		writeDistributionError(w, http.StatusServiceUnavailable)
		return
	}
	messages := make([]DistributionMessage, 0, minInt(limit, len(actions)))
	defer func() {
		for index := range messages {
			wipeDistributionMessage(&messages[index])
		}
	}()
	selectedKeys := make([]string, 0, cap(messages))
	encodedBytes := 256
	for _, action := range actions {
		if len(selectedKeys) >= limit {
			break
		}
		candidate := action.Message
		if action.SealedCertificate != nil {
			bundle, openErr := OpenParts(r.Context(), h.keys, action.MasterKeyReference, action.SealedCertificate, action.SealedPrivateKey)
			if openErr != nil {
				writeDistributionError(w, http.StatusServiceUnavailable)
				return
			}
			candidate.CertificatePEM, candidate.PrivateKeyPEM = bundle.CertificatePEM, bundle.PrivateKeyPEM
		}
		candidateJSON, marshalErr := json.Marshal(candidate)
		clear(candidateJSON)
		if marshalErr != nil {
			wipeDistributionMessage(&candidate)
			writeDistributionError(w, http.StatusServiceUnavailable)
			return
		}
		// Reserve a conservative envelope margin for version, cursor, commas, JSON escaping, and the Encoder
		// newline. A candidate that cannot fit alone is rejected at enqueue, so this always makes progress.
		if encodedBytes+len(candidateJSON)+1 > maxDistributionBody-4096 {
			wipeDistributionMessage(&candidate)
			if len(messages) > 0 {
				break
			}
			writeDistributionError(w, http.StatusServiceUnavailable)
			return
		}
		encodedBytes += len(candidateJSON) + 1
		messages = append(messages, candidate)
		selectedKeys = append(selectedKeys, action.Key)
	}
	complete := len(selectedKeys) == len(actions)
	nextCursor := ""
	if !complete && len(messages) > 0 {
		nextCursor = encodeDistributionCursor(selectedKeys[len(selectedKeys)-1])
	}
	writeDistributionJSON(w, http.StatusOK, distributionPage{Version: 1, Complete: complete, NextCursor: nextCursor, Messages: messages})
}

// DisabledDistributionHandler serves the distribution routes while managed
// certificates are disabled, so the edge's pull is a known route. Nothing can
// be pending, so pull answers the empty complete page; ack and request have no
// certificate to act on and answer unavailable.
func DisabledDistributionHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+CertificateDistributionPullPath, func(w http.ResponseWriter, _ *http.Request) {
		writeDistributionJSON(w, http.StatusOK, distributionPage{Version: 1, Complete: true, Messages: []DistributionMessage{}})
	})
	unavailable := func(w http.ResponseWriter, _ *http.Request) { writeDistributionError(w, http.StatusServiceUnavailable) }
	mux.HandleFunc("POST "+CertificateDistributionAckPath, unavailable)
	mux.HandleFunc("POST "+CertificateDistributionRequestPath, unavailable)
	return mux
}

func (h *DistributionHub) handleRequest(w http.ResponseWriter, r *http.Request) {
	var input distributionRequest
	if !decodeDistributionBody(w, r, &input) {
		return
	}
	identity, ok := distributionNodeIdentityFromContext(r.Context())
	if !ok || !validDistributionIdentity(identity.NodeID, identity.ProcessEpoch) {
		writeDistributionError(w, http.StatusForbidden)
		return
	}
	hostname, wildcard, err := normalizeHostname(input.Hostname)
	if err != nil || wildcard || hostname != input.Hostname {
		writeDistributionError(w, http.StatusBadRequest)
		return
	}
	if h == nil || h.onDemand == nil {
		writeDistributionError(w, http.StatusServiceUnavailable)
		return
	}
	if err := h.awaitOnDemand(r.Context(), identity, hostname); err != nil {
		if clientClosed(r, err) {
			w.WriteHeader(statusClientClosedRequest)
			return
		}
		// Only a wait that ran out is "not yet" here: a 409 like the other expected outcomes, never a reported outage.
		if pureDistributionError(err, ErrInvalid) || pureDistributionError(err, ErrGenerationConflict) || pureDistributionError(err, ErrCertificateRevoked) ||
			issuancePending(err) {
			writeDistributionError(w, http.StatusConflict)
			return
		}
		captureDistributionAuthorizationFailure(r, "control_unavailable", err)
		writeDistributionError(w, http.StatusServiceUnavailable)
		return
	}
	writeDistributionJSON(w, http.StatusNoContent, nil)
}

func (h *DistributionHub) handleAck(w http.ResponseWriter, r *http.Request) {
	var input distributionAck
	if !decodeDistributionBody(w, r, &input) || !validDistributionIdentity(input.EdgeNodeID, input.EdgeProcessEpoch) {
		return
	}
	identity, ok := distributionNodeIdentityFromContext(r.Context())
	if !ok || input.EdgeNodeID != identity.NodeID || input.EdgeProcessEpoch != identity.ProcessEpoch {
		writeDistributionError(w, http.StatusForbidden)
		return
	}
	if input.Version != 1 || input.Action != "stage" && input.Action != "activate" && input.Action != "retire" && input.Action != "revoke" || input.Status != "ready" && input.Status != "active" && input.Status != "retired" && input.Status != "revoked" && input.Status != "failed" || !distributionAckStatusMatchesAction(input.Action, input.Status) {
		writeDistributionError(w, http.StatusBadRequest)
		return
	}
	if input.Status == "failed" && (input.Code == "" || len(input.Code) > 128 || strings.ContainsAny(input.Code, "\r\n\x00")) || input.Status != "failed" && input.Code != "" {
		writeDistributionError(w, http.StatusBadRequest)
		return
	}
	key := distributionStoreKey(distributionAckKey(input))
	action, found, err := h.store.Get(r.Context(), key)
	if err != nil {
		writeDistributionError(w, http.StatusServiceUnavailable)
		return
	}
	if !found || action.Status != "" {
		if found && action.Status == input.Status {
			writeDistributionJSON(w, http.StatusNoContent, nil)
			return
		}
		writeDistributionError(w, http.StatusConflict)
		return
	}
	if !ackMatchesMessage(input, action.Message) {
		writeDistributionError(w, http.StatusConflict)
		return
	}
	completed, err := h.store.Complete(r.Context(), key, input.Status, input.Code, h.now().UTC())
	if err != nil {
		writeDistributionError(w, http.StatusServiceUnavailable)
		return
	}
	if !completed {
		// Another instance recorded a result first, or the action expired: same answer as a replay.
		if again, ok, getErr := h.store.Get(r.Context(), key); getErr == nil && ok && again.Status == input.Status {
			writeDistributionJSON(w, http.StatusNoContent, nil)
			return
		}
		writeDistributionError(w, http.StatusConflict)
		return
	}
	writeDistributionJSON(w, http.StatusNoContent, nil)
}

func (h *DistributionHub) authorized(r *http.Request) (bool, error) {
	if h == nil || r == nil {
		return false, nil
	}
	credential := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	h.credentialMu.RLock()
	active := len(h.credential) >= 32
	managed := active && hmac.Equal([]byte(credential), h.credential)
	h.credentialMu.RUnlock()
	if managed {
		return true, nil
	}
	if !active {
		return false, nil
	}
	return nodeauth.Matches(r.Context(), h.nodeCredentials, r, "", r.Header.Get("X-Paperboat-Edge-Node-ID"))
}

type distributionOperationFailure struct{ cause error }

func (e distributionOperationFailure) Error() string { return ErrDistributionTransportFailed.Error() }
func (e distributionOperationFailure) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrDistributionTransportFailed}
	}
	return []error{ErrDistributionTransportFailed, e.cause}
}

func distributionFailure(cause error) error { return distributionOperationFailure{cause: cause} }

func captureDistributionAuthorizationFailure(r *http.Request, code string, cause error) {
	if r == nil || cause == nil {
		return
	}
	ctx := r.Context()
	ctx = supportreport.WithFailureIdentity(ctx, "", serverdiagnostics.RequestID(ctx))
	supportreport.CaptureFromContext(ctx, "paperboat-server", "http_edge", "http_handler", code, supportreport.WithDiagnosticStatus(http.StatusServiceUnavailable, cause))
}

func pureDistributionError(err, expected error) (matched bool) {
	defer func() {
		if recover() != nil {
			matched = false
		}
	}()
	if err == nil || expected == nil {
		return false
	}
	nodes := 0
	var visit func(error, int) bool
	visit = func(current error, depth int) bool {
		if current == nil || depth >= 8 || nodes >= 16 {
			return false
		}
		typ := reflect.TypeOf(current)
		value := reflect.ValueOf(current)
		if typ == nil || (value.Kind() == reflect.Ptr || value.Kind() == reflect.Interface || value.Kind() == reflect.Map || value.Kind() == reflect.Slice || value.Kind() == reflect.Func) && value.IsNil() {
			return false
		}
		nodes++
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return false
			}
			for _, child := range children {
				if !visit(child, depth+1) {
					return false
				}
			}
			return true
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			child := wrapped.Unwrap()
			return child != nil && visit(child, depth+1)
		}
		expectedType := reflect.TypeOf(expected)
		return typ.Comparable() && expectedType != nil && expectedType.Comparable() && typ == expectedType && current == expected
	}
	return visit(err, 0)
}

func (h *DistributionHub) sign(ctx context.Context, message DistributionMessage) (string, error) {
	message.Proof = ""
	canonical, err := json.Marshal(message)
	if err != nil {
		return "", ErrDistributionTransportFailed
	}
	defer clear(canonical)
	h.credentialMu.RLock()
	credential := append([]byte(nil), h.credential...)
	h.credentialMu.RUnlock()
	defer clear(credential)
	if h.nodeCredentials != nil {
		// Both hosted and self-hosted nodes may own scoped credentials. A known but
		// unavailable scoped identity must never fall back to the fleet credential.
		value, err := h.nodeCredentials.RuntimeCredential(ctx, message.EdgeNodeID)
		if err == nil {
			clear(credential)
			credential = []byte(value)
			defer clear(credential)
		} else {
			if !nodeauth.IsNotFound(err) {
				return "", distributionFailure(err)
			}
			private, lookupErr := h.nodeCredentials.IsSelfHosted(ctx, message.EdgeNodeID)
			if lookupErr != nil {
				return "", distributionFailure(lookupErr)
			}
			if private {
				return "", ErrDistributionTransportAuth
			}
		}
	}
	if len(credential) < 32 {
		return "", ErrDistributionTransportAuth
	}
	hash := hmac.New(sha256.New, credential)
	_, _ = hash.Write(canonical)
	return base64.RawURLEncoding.EncodeToString(hash.Sum(nil)), nil
}

func VerifyDistributionMessageProof(message DistributionMessage, credential []byte) error {
	if len(credential) < 32 || message.Proof == "" {
		return ErrDistributionTransportAuth
	}
	proof, err := base64.RawURLEncoding.Strict().DecodeString(message.Proof)
	if err != nil || len(proof) != sha256.Size {
		return ErrDistributionTransportAuth
	}
	defer clear(proof)
	message.Proof = ""
	canonical, err := json.Marshal(message)
	if err != nil {
		return ErrDistributionTransportAuth
	}
	defer clear(canonical)
	hash := hmac.New(sha256.New, credential)
	_, _ = hash.Write(canonical)
	if !hmac.Equal(proof, hash.Sum(nil)) {
		return ErrDistributionTransportAuth
	}
	return nil
}

func validDistributionCredential(credential []byte) bool {
	return len(credential) >= 32 && bytes.Equal(bytes.TrimSpace(credential), credential) && bytes.IndexByte(credential, '\r') < 0 && bytes.IndexByte(credential, '\n') < 0 && bytes.IndexByte(credential, 0) < 0
}

func distributionKey(action string, request DistributionRequest) string {
	return distributionMessageKey(DistributionMessage{Action: action, CertificateID: request.Certificate.ID, AccountID: request.Certificate.AccountID, TunnelID: request.Certificate.TunnelID, DomainID: request.Certificate.DomainID, TargetKind: string(request.Certificate.TargetKind), RouteID: request.Certificate.RouteID, PreviewID: request.Certificate.PreviewID, PreviewGeneration: request.Certificate.PreviewGeneration, PreviewState: request.Certificate.PreviewState, PreviewExpiresAt: request.Certificate.PreviewExpiresAt, Hostname: request.Certificate.Hostname, DomainGeneration: request.Certificate.DomainGeneration, CertificateGeneration: request.Certificate.CertificateGeneration, EdgeNodeID: request.Target.NodeID, EdgeProcessEpoch: request.Target.ProcessEpoch, AssignmentGeneration: request.Target.Generation, Fingerprint: base64.RawURLEncoding.EncodeToString(request.Certificate.Fingerprint[:])})
}

func distributionMessageKey(message DistributionMessage) string {
	return strings.Join([]string{message.Action, message.CertificateID, message.AccountID, message.TunnelID, message.DomainID, message.TargetKind, message.RouteID, message.PreviewID, fmt.Sprint(message.PreviewGeneration), message.PreviewState, message.PreviewExpiresAt.UTC().Format(time.RFC3339Nano), message.Hostname, fmt.Sprint(message.DomainGeneration), fmt.Sprint(message.CertificateGeneration), message.EdgeNodeID, message.EdgeProcessEpoch, fmt.Sprint(message.AssignmentGeneration), message.Fingerprint}, "\x00")
}

func distributionAckKey(input distributionAck) string {
	return distributionMessageKey(DistributionMessage{Action: input.Action, CertificateID: input.CertificateID, AccountID: input.AccountID, TunnelID: input.TunnelID, DomainID: input.DomainID, TargetKind: input.TargetKind, RouteID: input.RouteID, PreviewID: input.PreviewID, PreviewGeneration: input.PreviewGeneration, PreviewState: input.PreviewState, PreviewExpiresAt: input.PreviewExpiresAt, Hostname: input.Hostname, DomainGeneration: input.DomainGeneration, CertificateGeneration: input.CertificateGeneration, EdgeNodeID: input.EdgeNodeID, EdgeProcessEpoch: input.EdgeProcessEpoch, AssignmentGeneration: input.AssignmentGeneration, Fingerprint: input.Fingerprint})
}

func ackMatchesMessage(input distributionAck, message DistributionMessage) bool {
	return input.Version == message.Version && input.Action == message.Action && input.CertificateID == message.CertificateID && input.AccountID == message.AccountID && input.TunnelID == message.TunnelID && input.DomainID == message.DomainID && input.TargetKind == message.TargetKind && input.RouteID == message.RouteID && input.PreviewID == message.PreviewID && input.PreviewGeneration == message.PreviewGeneration && input.PreviewState == message.PreviewState && input.PreviewExpiresAt.Equal(message.PreviewExpiresAt) && input.Hostname == message.Hostname && input.DomainGeneration == message.DomainGeneration && input.CertificateGeneration == message.CertificateGeneration && input.EdgeNodeID == message.EdgeNodeID && input.EdgeProcessEpoch == message.EdgeProcessEpoch && input.AssignmentGeneration == message.AssignmentGeneration && input.Fingerprint == message.Fingerprint
}

func wipeDistributionMessage(message *DistributionMessage) {
	if message == nil {
		return
	}
	clear(message.CertificatePEM)
	clear(message.PrivateKeyPEM)
	message.CertificatePEM = nil
	message.PrivateKeyPEM = nil
}

func distributionMessageFitsResponse(message DistributionMessage) bool {
	encoded, err := json.Marshal(message)
	if err != nil {
		return false
	}
	length := len(encoded)
	clear(encoded)
	return length > 0 && length+4096 <= maxDistributionBody
}

func validDistributionIdentity(nodeID, processEpoch string) bool {
	return validIdentifier(nodeID) && validEpoch(processEpoch)
}

func validDistributionCursor(cursor string) bool {
	return len(cursor) <= 4096 && (cursor == "" || strings.Trim(cursor, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_") == "")
}

func decodeDistributionCursor(cursor string) (string, bool) {
	if cursor == "" {
		return "", true
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
	if err != nil || len(decoded) > 2048 || bytes.IndexByte(decoded, '\r') >= 0 || bytes.IndexByte(decoded, '\n') >= 0 {
		return "", false
	}
	return string(decoded), true
}

func encodeDistributionCursor(cursor string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursor))
}

func distributionAckStatusMatchesAction(action, status string) bool {
	if status == "failed" {
		return true
	}
	switch action {
	case "stage":
		return status == "ready"
	case "activate":
		return status == "active"
	case "retire":
		return status == "retired"
	case "revoke":
		return status == "revoked"
	default:
		return false
	}
}

func decodeDistributionBody(w http.ResponseWriter, r *http.Request, target any) bool {
	if r == nil || r.Body == nil || !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		writeDistributionError(w, http.StatusBadRequest)
		return false
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxDistributionBody+1))
	if err != nil || len(body) == 0 || len(body) > maxDistributionBody {
		writeDistributionError(w, http.StatusBadRequest)
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		writeDistributionError(w, http.StatusBadRequest)
		return false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		writeDistributionError(w, http.StatusBadRequest)
		return false
	}
	return true
}

func writeDistributionJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if value != nil {
		payload, err := json.Marshal(value)
		if err != nil {
			return
		}
		wire := make([]byte, len(payload)+1)
		copy(wire, payload)
		wire[len(payload)] = '\n'
		clear(payload)
		defer clear(wire)
		_, _ = w.Write(wire)
	}
}

func writeDistributionError(w http.ResponseWriter, status int) {
	writeDistributionJSON(w, status, map[string]any{"code": "certificate_distribution_unavailable", "retryable": status >= 500})
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

type distributionNodeIdentityContextKey struct{}

func withDistributionNodeIdentity(ctx context.Context, identity DistributionNodeIdentity) context.Context {
	return context.WithValue(ctx, distributionNodeIdentityContextKey{}, identity)
}

func distributionNodeIdentityFromContext(ctx context.Context) (DistributionNodeIdentity, bool) {
	if ctx == nil {
		return DistributionNodeIdentity{}, false
	}
	identity, ok := ctx.Value(distributionNodeIdentityContextKey{}).(DistributionNodeIdentity)
	return identity, ok
}

func distributionExpiry(now, certificateExpiry time.Time) time.Time {
	deadline := now.Add(maxDistributionLifetime)
	if certificateExpiry.IsZero() || certificateExpiry.Before(deadline) {
		return certificateExpiry
	}
	return deadline
}

func distributionContextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

// statusClientClosedRequest answers a request whose client went away first (nginx's 499). An edge's on-demand
// certificate request runs inside a client's TLS handshake and is cancelled when that handshake ends; that is not
// a control-plane failure and must not be reported as a 503.
const statusClientClosedRequest = 499

func clientClosed(r *http.Request, err error) bool {
	return r.Context().Err() != nil && errors.Is(err, r.Context().Err())
}

// onDemandWait bounds how long a request waits for an issuance another request already started.
var onDemandWait = 25 * time.Second

func issuancePending(err error) bool {
	return pureDistributionError(err, ErrIssuanceLocked) || pureDistributionError(err, ErrCertificateNotReady) || pureDistributionError(err, ErrDNSChallengePending)
}

// awaitOnDemand asks for the leaf; when another request (the other edge, or another server instance) is already
// issuing it, it waits for that issuance and asks again, so every concurrent first handshake gets the one
// certificate. The issuance lock is in the shared database, so the wait polls it with backoff rather than an
// in-process channel. Prior art: certmagic's on-demand obtain waits on the in-flight issuance for the same name
// (handshake.go obtainOnDemandCertificate, obtainCertWaitChans, bounded wait). Only a wait that runs out is returned.
func (h *DistributionHub) awaitOnDemand(parent context.Context, identity DistributionNodeIdentity, hostname string) error {
	ctx, cancel := context.WithTimeout(parent, onDemandWait)
	defer cancel()
	delay := 250 * time.Millisecond
	for {
		err := h.onDemand(ctx, identity, hostname)
		if err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			if parent.Err() != nil {
				return parent.Err() // the edge went away: the handler answers 499
			}
			return ErrCertificateNotReady // our own wait ran out inside the call: still "not yet"
		}
		if err == nil || !issuancePending(err) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
		delay = min(2*delay, 2*time.Second)
	}
}
