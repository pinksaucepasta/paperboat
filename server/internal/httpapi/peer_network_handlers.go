package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/peersessions"
	"github.com/pinksaucepasta/paperboat/server/internal/ratelimit"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

type peerNetworkRegisterBody struct {
	OperationID                string `json:"operation_id"`
	DaemonGeneration           string `json:"daemon_generation"`
	WireGuardPublicKey         string `json:"wireguard_public_key"`
	DiscoPublicKey             string `json:"disco_public_key"`
	ExpectedKeyGeneration      int64  `json:"expected_key_generation"`
	QUICCertificateFingerprint string `json:"quic_certificate_fingerprint"`
}
type peerNetworkConfigBody struct {
	OperationID          string                             `json:"operation_id"`
	DaemonGeneration     string                             `json:"daemon_generation"`
	AdmittedScopes       []peersessions.AdmittedScope       `json:"admitted_scopes,omitempty"`
	AdmittedCertificates []peersessions.AdmittedCertificate `json:"admitted_certificates,omitempty"`
	// Endpoints are the engine's own candidate endpoints (ip:port, at most 8; PLAN 3b N5), sent once a config
	// answer said endpoint_reports.
	Endpoints []string `json:"endpoints,omitempty"`
	// Authority names the peer-state semantics the daemon understands: "" (every scope expires with the document)
	// or peersessions.AuthorityUntilRevoked (PLAN 3b offline-capable authority, F21-01/F21-11).
	Authority string `json:"authority,omitempty"`
}
type peerNetworkService interface {
	Register(context.Context, peersessions.NetworkRegistration) (peersessions.NetworkRegistrationResult, error)
	Configuration(context.Context, peersessions.NetworkConfigRequest) (peersessions.NetworkConfigResult, error)
	ReportCarrierOffer(context.Context, peersessions.CarrierOfferReport) error
	BeginDrain(ctx context.Context, userID, ownerEndpointID, endpointID string) error
	Withdraw(ctx context.Context, userID, ownerEndpointID, endpointID string) error
}

// daemonEndpoint is the network endpoint of the calling daemon generation of the CLI session.
func daemonEndpoint(w http.ResponseWriter, r *http.Request, generation string) (principal, string, bool) {
	p, ok := principalFromContext(r.Context())
	if !ok || p.Client == nil {
		writeError(w, r, http.StatusUnauthorized, "authentication_required", "CLI authentication is required.")
		return p, "", false
	}
	endpoint, ok := peernetwork.DaemonEndpointID(p.Client.SessionID, generation)
	if !ok {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "The daemon generation must be 32 lowercase hex characters.")
		return p, "", false
	}
	return p, endpoint, true
}

func decodePeerNetworkRegister(body peerNetworkRegisterBody) ([]byte, []byte, []byte, bool) {
	key, e1 := base64.RawURLEncoding.Strict().DecodeString(body.WireGuardPublicKey)
	disco, e2 := base64.RawURLEncoding.Strict().DecodeString(body.DiscoPublicKey)
	fingerprint, e3 := hex.DecodeString(body.QUICCertificateFingerprint)
	return key, disco, fingerprint, e1 == nil && e2 == nil && e3 == nil && len(key) == 32 && len(disco) == 32 && len(fingerprint) == 32 && base64.RawURLEncoding.EncodeToString(key) == body.WireGuardPublicKey && base64.RawURLEncoding.EncodeToString(disco) == body.DiscoPublicKey && hex.EncodeToString(fingerprint) == body.QUICCertificateFingerprint
}
func peerNetworkError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, peersessions.ErrGenerationLimit):
		writeError(w, r, http.StatusConflict, "peer_network_generation_limit", "Four pb daemon generations of this sign-in may still hold connections. Wait for an older one to finish draining (pb doctor lists them), or stop it, then retry.")
	case errors.Is(err, peersessions.ErrGenerationEvicted):
		writeError(w, r, http.StatusForbidden, "peer_network_generation_evicted", "A newer pb daemon generation replaced this one; the streams it still held have ended.")
	case errors.Is(err, peersessions.ErrGenerationWithdrawn):
		writeError(w, r, http.StatusForbidden, "peer_network_generation_withdrawn", "This pb daemon generation has finished; the current daemon serves new connections.")
	case errors.Is(err, peersessions.ErrConflict):
		writeError(w, r, http.StatusConflict, "peer_network_conflict", "The peer network key generation changed. Refresh registration state before retrying rotation.")
	case errors.Is(err, peersessions.ErrDenied):
		writeError(w, r, http.StatusForbidden, "peer_network_denied", "Current endpoint authority no longer permits peer networking. Remove installed peers and reauthorize the endpoint.")
	case errors.Is(err, peersessions.ErrResourceLimit):
		writeError(w, r, http.StatusConflict, "peer_network_too_large", "This account has more computers than one peer network configuration can carry. Remove computers you no longer use (pb machine list), then retry.")
	case errors.Is(err, peersessions.ErrInvalid):
		writeError(w, r, http.StatusBadRequest, "invalid_request", "Peer network request is invalid.")
	default:
		// The causes here are package sentinels (no keys or addresses), and the
		// step that failed is otherwise invisible in the error-type chain.
		slog.Warn("peer network configuration unavailable", "error", err.Error())
		writeError(w, r, http.StatusServiceUnavailable, "peer_network_unavailable", "Peer network authority is temporarily unavailable. Retry before the current configuration expires.", err)
	}
}

func peerNetworkRegister(s peerNetworkService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body peerNetworkRegisterBody
		if !decodeStrictJSON(w, r, &body) {
			return
		}
		p, endpoint, ok := daemonEndpoint(w, r, body.DaemonGeneration)
		if !ok {
			return
		}
		key, disco, fingerprint, valid := decodePeerNetworkRegister(body)
		if !valid {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Peer network registration is invalid.")
			return
		}
		value, err := s.Register(r.Context(), peersessions.NetworkRegistration{OperationID: body.OperationID, UserID: p.User.ID, EndpointID: endpoint, OwnerEndpointID: p.Client.SessionID, Role: "cli", EndpointGeneration: 1, ExpectedKeyGeneration: body.ExpectedKeyGeneration, WireGuardPublicKey: key, DiscoPublicKey: disco, QUICCertificateFingerprint: fingerprint})
		if err != nil {
			peerNetworkError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: value})
	}
}
func peerNetworkConfig(s peerNetworkService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body peerNetworkConfigBody
		if !decodeStrictJSON(w, r, &body) {
			return
		}
		p, endpoint, ok := daemonEndpoint(w, r, body.DaemonGeneration)
		if !ok {
			return
		}
		value, err := s.Configuration(r.Context(), peersessions.NetworkConfigRequest{OperationID: body.OperationID, UserID: p.User.ID, EndpointID: endpoint, AdmittedScopes: body.AdmittedScopes, AdmittedCertificates: body.AdmittedCertificates, Endpoints: body.Endpoints, Authority: body.Authority})
		if err != nil {
			peerNetworkError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: value})
	}
}

type peerNetworkGenerationBody struct {
	DaemonGeneration string `json:"daemon_generation"`
}

// peerNetworkGeneration serves a daemon generation's own lifecycle step: drain (it stopped accepting and keeps what
// it holds, up to runtimeattachment.DrainLimit) or withdraw (it finished). Both are idempotent.
func peerNetworkGeneration(step func(context.Context, string, string, string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body peerNetworkGenerationBody
		if !decodeStrictJSON(w, r, &body) {
			return
		}
		p, endpoint, ok := daemonEndpoint(w, r, body.DaemonGeneration)
		if !ok {
			return
		}
		if err := step(r.Context(), p.User.ID, p.Client.SessionID, endpoint); err != nil {
			peerNetworkError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]bool{"ok": true}})
	}
}

func machinePeerNetworkRegister(s peerNetworkService, v machineEndpointProofVerifier) http.HandlerFunc {
	return machinePeerNetwork(s, v, true, nil)
}

// machinePeerNetworkConfig answers a machine's configuration refresh. limiter bounds refreshes per machine credential,
// before any database work: a client that loops (2-6 requests a second per machine, 2026-10-26) gets 429 with
// Retry-After instead of loading the control plane.
func machinePeerNetworkConfig(s peerNetworkService, v machineEndpointProofVerifier, limiter *ratelimit.Limiter) http.HandlerFunc {
	return machinePeerNetwork(s, v, false, limiter)
}

type machineCarrierOfferBody struct {
	OperationID string `json:"operation_id"`
	// Offer is the machine's active carrier generation offer (native.CarrierOffer bytes); empty withdraws it.
	Offer []byte `json:"offer"`
}

// machineCarrierOffer stores a machine's carrier generation offer for its peers' configurations. It has its own
// limiter: a report is rare (one per generation and renewal) and must not wait behind configuration refreshes.
func machineCarrierOffer(s peerNetworkService, v machineEndpointProofVerifier, limiter *ratelimit.Limiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, raw, ok := verifyMachinePeerNetwork(w, r, v, limiter)
		if !ok {
			return
		}
		var body machineCarrierOfferBody
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&body) != nil || len(body.Offer) > peersessions.MaxCarrierOfferBytes {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Carrier offer report is invalid.")
			return
		}
		if err := s.ReportCarrierOffer(r.Context(), peersessions.CarrierOfferReport{UserID: claims.UserID, MachineID: claims.MachineID, MachineGeneration: claims.InstallationGeneration, Offer: body.Offer}); err != nil {
			peerNetworkError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: struct{}{}})
	}
}

// verifyMachinePeerNetwork reads a machine-proof request body and verifies it: the per-credential limiter first
// (before any proof or database work), then the proof over the exact body and its operation id.
func verifyMachinePeerNetwork(w http.ResponseWriter, r *http.Request, v machineEndpointProofVerifier, limiter *ratelimit.Limiter) (controlplane.MachineRequestClaims, []byte, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxMachineControlRequest+1))
	if err != nil || len(raw) > maxMachineControlRequest {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "Peer network request is invalid.")
		return controlplane.MachineRequestClaims{}, nil, false
	}
	var operation struct {
		OperationID string `json:"operation_id"`
	}
	if json.Unmarshal(raw, &operation) != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "Peer network request is invalid.")
		return controlplane.MachineRequestClaims{}, nil, false
	}
	proof, e1 := base64.RawURLEncoding.Strict().DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
	scheme, credential, found := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if e1 != nil || len(proof) == 0 || !found || !strings.EqualFold(scheme, "Bearer") {
		writeError(w, r, http.StatusUnauthorized, "machine_identity_invalid", "Machine identity proof was rejected.")
		return controlplane.MachineRequestClaims{}, nil, false
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(credential)))
	if allowed, wait := limiter.Allow(hex.EncodeToString(sum[:])); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
		writeError(w, r, http.StatusTooManyRequests, "rate_limited", "Peer network requests are sent too often. Retry after the delay.")
		return controlplane.MachineRequestClaims{}, nil, false
	}
	claims, e2 := v.VerifyMachineRequest(r.Context(), strings.TrimSpace(credential), proof, r.Method, r.URL.Path, raw)
	if e2 != nil {
		writeMachineProofFailure(w, r, e2, "machine_identity_invalid", "Machine identity proof was rejected.")
		return controlplane.MachineRequestClaims{}, nil, false
	}
	if claims.OperationID != operation.OperationID {
		logHTTPRejection(r, http.StatusUnauthorized, "machine_identity_invalid", nil)
		writeError(w, r, http.StatusUnauthorized, "machine_identity_invalid", "Machine identity proof was rejected.")
		return controlplane.MachineRequestClaims{}, nil, false
	}
	return claims, raw, true
}

func machinePeerNetwork(s peerNetworkService, v machineEndpointProofVerifier, register bool, limiter *ratelimit.Limiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, raw, ok := verifyMachinePeerNetwork(w, r, v, limiter)
		if !ok {
			return
		}
		if register {
			var body peerNetworkRegisterBody
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&body) != nil {
				writeError(w, r, http.StatusBadRequest, "invalid_request", "Peer network registration is invalid.")
				return
			}
			key, disco, fingerprint, valid := decodePeerNetworkRegister(body)
			if !valid {
				writeError(w, r, http.StatusBadRequest, "invalid_request", "Peer network registration is invalid.")
				return
			}
			value, e := s.Register(r.Context(), peersessions.NetworkRegistration{OperationID: body.OperationID, UserID: claims.UserID, EndpointID: claims.MachineID, OwnerEndpointID: claims.MachineID, Role: "machine", MachineID: claims.MachineID, EndpointGeneration: claims.InstallationGeneration, MachineGeneration: claims.InstallationGeneration, ExpectedKeyGeneration: body.ExpectedKeyGeneration, WireGuardPublicKey: key, DiscoPublicKey: disco, QUICCertificateFingerprint: fingerprint})
			if e != nil {
				peerNetworkError(w, r, e)
				return
			}
			writeJSON(w, http.StatusOK, SuccessResponse{Data: value})
			return
		}
		var body peerNetworkConfigBody
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&body) != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Peer network configuration request is invalid.")
			return
		}
		value, e := s.Configuration(r.Context(), peersessions.NetworkConfigRequest{OperationID: body.OperationID, UserID: claims.UserID, EndpointID: claims.MachineID, AdmittedScopes: body.AdmittedScopes, AdmittedCertificates: body.AdmittedCertificates, Endpoints: body.Endpoints, Authority: body.Authority})
		if e != nil {
			peerNetworkError(w, r, e)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: value})
	}
}
