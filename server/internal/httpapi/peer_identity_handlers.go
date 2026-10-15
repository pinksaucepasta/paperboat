package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	wire "github.com/pinksaucepasta/paperboat/shared/peeridentity"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/peeridentity"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
)

type endpointCertificateRegistrar interface {
	Register(context.Context, peeridentity.RegisterRequest) (wire.Certificate, error)
}

type e2eeBootstrapper interface {
	Bootstrap(context.Context, peeridentity.BootstrapRequest) (wire.Certificate, error)
}

type e2eeRootReader interface {
	Root(context.Context, string) (peeridentity.AccountRoot, error)
}

type transportKeyReader interface {
	TransportKeys(context.Context, string, time.Time) (peeridentity.AccountRoot, error)
}

type endpointCertificateReader interface {
	Get(context.Context, string, string, uint64, time.Time) (wire.Certificate, error)
}

type endpointCertificateRevoker interface {
	Revoke(context.Context, string, string, string, uint64, uint64, string, time.Time) (wire.Certificate, error)
}

func e2eeRootGet(service e2eeRootReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok || principal.Client == nil {
			writeError(w, r, http.StatusUnauthorized, "authentication_required", "CLI authentication is required.")
			return
		}
		root, err := service.Root(r.Context(), principal.User.ID)
		if err != nil {
			status, code := http.StatusServiceUnavailable, "temporarily_unavailable"
			switch {
			case expectedHTTPErrorOnly(err, peeridentity.ErrUnavailable):
				status, code = http.StatusNotFound, "not_found"
			case expectedHTTPErrorOnly(err, wire.ErrInvalid):
				status, code = http.StatusBadRequest, "invalid_request"
			}
			logIdentityRejection(r, status, err)
			writeError(w, r, status, code, "E2EE root could not be retrieved.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: wire.RootDocument{Version: 1, TrustedKeys: trustedKeyDocuments(root)}})
	}
}

func transportKeysGet(service transportKeyReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok || principal.Client == nil {
			writeError(w, r, http.StatusUnauthorized, "authentication_required", "CLI authentication is required.")
			return
		}
		keys, err := service.TransportKeys(r.Context(), principal.User.ID, time.Now().UTC())
		if err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "temporarily_unavailable", "Machine trust could not be retrieved.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: wire.RootDocument{Version: 1, TrustedKeys: trustedKeyDocuments(keys)}})
	}
}

func e2eeBootstrap(service e2eeBootstrapper) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok || principal.Client == nil {
			writeError(w, r, http.StatusUnauthorized, "authentication_required", "CLI authentication is required.")
			return
		}
		var document wire.BootstrapRequestDocument
		if !decodeStrictJSON(w, r, &document) {
			return
		}
		operationID := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		rootPublic, rootErr := decodeCanonicalBase64URL(document.RootPublicKey)
		certificate, certificateErr := decodeCanonicalBase64URL(document.Certificate.Certificate)
		rootFingerprint, rootFingerprintErr := peeridentity.FingerprintForKeyID(document.Certificate.KeyID)
		certificateFingerprint, fingerprintErr := decodeFingerprint(document.Certificate.CertificateFingerprint)
		rootKeyID, rootKeyIDErr := peeridentity.KeyID(rootPublic)
		issuedAt, issuedErr := parseCanonicalTime(document.Certificate.IssuedAt)
		expiresAt, expiresErr := parseCanonicalTime(document.Certificate.ExpiresAt)
		if stage := e2eeBootstrapValidationStage(operationID, document, principal, rootPublic, rootErr, rootKeyID, rootKeyIDErr, certificateErr, rootFingerprintErr, fingerprintErr, issuedErr, expiresErr); stage != "" {
			slog.Warn("E2EE bootstrap request rejected", "request_id", requestIDFromContext(r.Context()), "stage", stage)
			writeErrorDetails(w, r, http.StatusBadRequest, "invalid_request", "E2EE bootstrap request is invalid.", map[string]any{"stage": stage})
			return
		}
		value, err := service.Bootstrap(r.Context(), peeridentity.BootstrapRequest{RegisterRequest: peeridentity.RegisterRequest{OperationID: operationID, UserID: principal.User.ID, KeyID: document.Certificate.KeyID, Certificate: certificate, Expected: wire.Expected{AccountID: principal.User.ID, Role: wire.RoleCLI, EndpointID: principal.Client.SessionID, Generation: 1, Serial: document.Certificate.Serial}, ExpectedRootFingerprint: rootFingerprint, ExpectedCertificateFingerprint: certificateFingerprint, ExpectedIssuedAt: issuedAt, ExpectedExpiresAt: expiresAt, Now: time.Now().UTC()}, CLIClientSessionID: principal.Client.SessionID, RootPublicKey: rootPublic, AllowRootReplacement: r.Header.Get("X-Paperboat-Fresh-Enrollment") == "1"})
		if err != nil {
			status, code := http.StatusServiceUnavailable, "temporarily_unavailable"
			switch {
			case expectedHTTPErrorOnly(err, peeridentity.ErrConflict):
				status, code = http.StatusConflict, "identity_conflict"
			case expectedHTTPErrorOnly(err, peeridentity.ErrUnavailable):
				status, code = http.StatusServiceUnavailable, "temporarily_unavailable"
			case expectedHTTPErrorOnly(err, wire.ErrNotCurrent):
				status, code = http.StatusBadRequest, "certificate_expired"
			case expectedHTTPErrorOnly(err, wire.ErrInvalid, wire.ErrIdentity, wire.ErrSignature):
				status, code = http.StatusBadRequest, "invalid_identity"
			default:
				status, code = http.StatusServiceUnavailable, "temporarily_unavailable"
			}
			class := e2eeBootstrapErrorClass(err)
			logIdentityRejection(r, status, err)
			writeErrorDetails(w, r, status, code, "E2EE identity could not be bootstrapped.", map[string]any{"stage": "service", "reason": class}, err)
			return
		}
		trustedKeys := []wire.TrustedKeyDocument{{KeyID: value.KeyID, PublicKey: base64.RawURLEncoding.EncodeToString(rootPublic), Fingerprint: hex.EncodeToString(rootFingerprint[:]), Generation: 1}}
		if r.Header.Get("X-Paperboat-Fresh-Enrollment") == "1" {
			if reader, ok := service.(transportKeyReader); ok {
				if keys, keysErr := reader.TransportKeys(r.Context(), principal.User.ID, time.Now().UTC()); keysErr == nil {
					trustedKeys = trustedKeyDocuments(keys)
				} else {
					ctx := supportreport.WithFailureIdentity(r.Context(), principal.User.ID, requestIDFromContext(r.Context()), supportReference(r.Context()))
					supportreport.ObserveFromContext(ctx, "paperboat-server", supportOperation(r.URL.Path), "service_error", "temporarily_unavailable", keysErr)
				}
			}
		} else if reader, ok := service.(e2eeRootReader); ok {
			if root, rootErr := reader.Root(r.Context(), principal.User.ID); rootErr == nil {
				trustedKeys = trustedKeyDocuments(root)
			} else {
				ctx := supportreport.WithFailureIdentity(r.Context(), principal.User.ID, requestIDFromContext(r.Context()), supportReference(r.Context()))
				supportreport.ObserveFromContext(ctx, "paperboat-server", supportOperation(r.URL.Path), "service_error", "temporarily_unavailable", rootErr)
			}
		}
		writeJSON(w, http.StatusCreated, SuccessResponse{Data: wire.BootstrapDocument{KeyID: value.KeyID, TrustedKeys: trustedKeys, Certificate: certificateDocument(value)}})
	}
}

func e2eeBootstrapValidationStage(operationID string, document wire.BootstrapRequestDocument, principal principal, rootPublic []byte, rootErr error, rootKeyID string, rootKeyIDErr, certificateErr, rootFingerprintErr, fingerprintErr, issuedErr, expiresErr error) string {
	switch {
	case operationID == "":
		return "idempotency_key"
	case rootErr != nil:
		return "root_public_key_encoding"
	case len(rootPublic) != 32:
		return "root_public_key_length"
	case rootKeyIDErr != nil:
		return "root_key_id_derivation"
	case rootKeyID != document.Certificate.KeyID:
		return "root_key_id_binding"
	case certificateErr != nil:
		return "certificate_encoding"
	case rootFingerprintErr != nil:
		return "root_fingerprint"
	case fingerprintErr != nil:
		return "certificate_fingerprint"
	case issuedErr != nil:
		return "issued_at"
	case expiresErr != nil:
		return "expires_at"
	case document.Certificate.Version != 1:
		return "version"
	case document.Certificate.AccountID != principal.User.ID:
		return "account_binding"
	case document.Certificate.EndpointID != principal.Client.SessionID:
		return "endpoint_binding"
	case document.Certificate.Role != "cli":
		return "role"
	case document.Certificate.Generation != 1:
		return "generation"
	case document.Certificate.Serial == 0:
		return "serial"
	default:
		return ""
	}
}

func e2eeBootstrapErrorClass(err error) string {
	switch {
	case expectedHTTPErrorOnly(err, peeridentity.ErrConflict):
		return "conflict"
	case expectedHTTPErrorOnly(err, peeridentity.ErrUnavailable):
		return "unavailable"
	case expectedHTTPErrorOnly(err, wire.ErrNotCurrent):
		return "not_current"
	case expectedHTTPErrorOnly(err, wire.ErrInvalid):
		return "invalid"
	case expectedHTTPErrorOnly(err, wire.ErrIdentity):
		return "identity"
	case expectedHTTPErrorOnly(err, wire.ErrSignature):
		return "signature"
	default:
		return "internal_failure"
	}
}

func endpointCertificateRegister(service endpointCertificateRegistrar) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "authentication_required", "Authentication is required.")
			return
		}
		var document wire.CertificateDocument
		if !decodeStrictJSON(w, r, &document) {
			return
		}
		operationID := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		generation, generationErr := strconv.ParseUint(r.PathValue("generation"), 10, 64)
		role := endpointRole(document.Role)
		certificate, certificateErr := decodeCanonicalBase64URL(document.Certificate)
		rootFingerprint, rootErr := peeridentity.FingerprintForKeyID(document.KeyID)
		certificateFingerprint, fingerprintErr := decodeFingerprint(document.CertificateFingerprint)
		issuedAt, issuedErr := parseCanonicalTime(document.IssuedAt)
		expiresAt, expiresErr := parseCanonicalTime(document.ExpiresAt)
		if operationID == "" || document.Version != 1 || document.AccountID != principal.User.ID ||
			document.EndpointID != r.PathValue("endpoint_id") || generationErr != nil || generation != document.Generation ||
			role.String() == "" || certificateErr != nil || rootErr != nil || fingerprintErr != nil || issuedErr != nil || expiresErr != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Endpoint certificate request is invalid.")
			return
		}
		value, err := service.Register(r.Context(), peeridentity.RegisterRequest{
			OperationID: operationID, UserID: principal.User.ID, KeyID: document.KeyID, Certificate: certificate,
			Expected:                wire.Expected{AccountID: document.AccountID, Role: role, EndpointID: document.EndpointID, Generation: document.Generation, Serial: document.Serial},
			ExpectedRootFingerprint: rootFingerprint, ExpectedCertificateFingerprint: certificateFingerprint,
			ExpectedIssuedAt: issuedAt, ExpectedExpiresAt: expiresAt, Now: time.Now().UTC(),
		})
		if err != nil {
			status, code := http.StatusServiceUnavailable, "temporarily_unavailable"
			switch {
			case expectedHTTPErrorOnly(err, peeridentity.ErrConflict):
				status, code = http.StatusConflict, "operation_conflict"
			case expectedHTTPErrorOnly(err, peeridentity.ErrUnavailable):
				status, code = http.StatusServiceUnavailable, "temporarily_unavailable"
			case expectedHTTPErrorOnly(err, wire.ErrNotCurrent):
				code = "certificate_expired"
				status = http.StatusBadRequest
			case expectedHTTPErrorOnly(err, wire.ErrInvalid, wire.ErrIdentity, wire.ErrSignature):
				status, code = http.StatusBadRequest, "invalid_certificate"
			}
			logIdentityRejection(r, status, err)
			writeError(w, r, status, code, "Endpoint certificate could not be registered.", err)
			return
		}
		writeJSON(w, http.StatusCreated, SuccessResponse{Data: certificateDocument(value)})
	}
}

func endpointCertificateGet(service endpointCertificateReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		generation, err := strconv.ParseUint(r.PathValue("generation"), 10, 64)
		if !ok || err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Endpoint certificate request is invalid.")
			return
		}
		value, err := service.Get(r.Context(), principal.User.ID, r.PathValue("endpoint_id"), generation, time.Now().UTC())
		if err != nil {
			status, code := http.StatusServiceUnavailable, "temporarily_unavailable"
			switch {
			case expectedHTTPErrorOnly(err, peeridentity.ErrUnavailable):
				status, code = http.StatusNotFound, "not_found"
			case expectedHTTPErrorOnly(err, wire.ErrInvalid):
				status, code = http.StatusBadRequest, "invalid_request"
			}
			logIdentityRejection(r, status, err)
			writeError(w, r, status, code, "Endpoint certificate could not be retrieved.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: certificateDocument(value)})
	}
}

func endpointCertificateRevoke(service endpointCertificateRevoker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		generation, generationErr := strconv.ParseUint(r.PathValue("generation"), 10, 64)
		operationID := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		serial, serialErr := parseQuotedSerial(r.Header.Get("If-Match"))
		if !ok || generationErr != nil || operationID == "" || serialErr != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Endpoint certificate revocation request is invalid.")
			return
		}
		value, err := service.Revoke(r.Context(), operationID, principal.User.ID, r.PathValue("endpoint_id"), generation, serial, "endpoint_removed", time.Now().UTC())
		if err != nil {
			status, code := http.StatusServiceUnavailable, "temporarily_unavailable"
			switch {
			case expectedHTTPErrorOnly(err, peeridentity.ErrConflict):
				status, code = http.StatusConflict, "operation_conflict"
			case expectedHTTPErrorOnly(err, peeridentity.ErrUnavailable):
				status, code = http.StatusNotFound, "not_found"
			case expectedHTTPErrorOnly(err, wire.ErrInvalid):
				status, code = http.StatusBadRequest, "invalid_request"
			}
			logIdentityRejection(r, status, err)
			writeError(w, r, status, code, "Endpoint certificate could not be revoked.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: certificateDocument(value)})
	}
}

func certificateDocument(value wire.Certificate) wire.CertificateDocument {
	return wire.CertificateDocument{
		Version: 1, AccountID: value.AccountID, KeyID: value.KeyID,
		EndpointID: value.EndpointID, Role: value.Role.String(), Generation: value.Generation, Serial: value.Serial,
		IssuedAt: value.IssuedAt.UTC().Format(time.RFC3339), ExpiresAt: value.ExpiresAt.UTC().Format(time.RFC3339),
		Certificate: base64.RawURLEncoding.EncodeToString(value.Raw), CertificateFingerprint: hex.EncodeToString(value.Fingerprint[:]),
	}
}

func trustedKeyDocuments(root peeridentity.AccountRoot) []wire.TrustedKeyDocument {
	documents := make([]wire.TrustedKeyDocument, 0, len(root.Keys))
	for _, key := range root.Keys {
		documents = append(documents, wire.TrustedKeyDocument{
			KeyID: key.KeyID, PublicKey: base64.RawURLEncoding.EncodeToString(key.PublicKey),
			Fingerprint: hex.EncodeToString(key.Fingerprint[:]), Generation: key.Generation,
		})
	}
	return documents
}

func endpointRole(value string) wire.Role {
	switch value {
	case "cli":
		return wire.RoleCLI
	case "machine":
		return wire.RoleMachine
	default:
		return 0
	}
}

func decodeCanonicalBase64URL(value string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return nil, errors.New("invalid base64url")
	}
	return decoded, nil
}

func decodeFingerprint(value string) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != len(result) || hex.EncodeToString(decoded) != value {
		return result, errors.New("invalid fingerprint")
	}
	copy(result[:], decoded)
	return result, nil
}

func parseCanonicalTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil || parsed.Nanosecond() != 0 || parsed.UTC().Format(time.RFC3339) != value {
		return time.Time{}, errors.New("invalid timestamp")
	}
	return parsed, nil
}

func parseQuotedSerial(value string) (uint64, error) {
	if len(value) < 3 || value[0] != '"' || value[len(value)-1] != '"' {
		return 0, errors.New("invalid serial etag")
	}
	return strconv.ParseUint(value[1:len(value)-1], 10, 64)
}

func logIdentityRejection(r *http.Request, status int, err error) {
	if status < http.StatusBadRequest || status >= http.StatusInternalServerError {
		return
	}
	code := "invalid_request"
	switch status {
	case http.StatusNotFound:
		code = "not_found"
	case http.StatusConflict:
		code = "operation_conflict"
	}
	logHTTPRejection(r, status, code, err)
}
