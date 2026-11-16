package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/managedssh"

	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/managedssh"
)

const maxManagedSSHRequest = 64 << 10

type managedSSHService interface {
	IssueCertificate(context.Context, managedssh.IssueCertificateRequest) (managedssh.Certificate, error)
	TrustedUserCAs(context.Context, managedssh.UserCAsRequest) (managedssh.TrustedUserCASet, error)
	RotateUserCA(context.Context, string, time.Time) ([]managedssh.UserCA, error)
	RegisterTarget(context.Context, managedssh.RegisterTargetRequest) (managedssh.MachineTarget, error)
	UpdateTargetPort(context.Context, managedssh.UpdateTargetPortRequest) (managedssh.MachineTarget, error)
	GetTarget(context.Context, managedssh.GetTargetRequest) (managedssh.MachineTarget, error)
	ObserveHost(context.Context, managedssh.ObserveHostRequest) (managedssh.HostKeySet, error)
	PromoteHost(context.Context, managedssh.PromoteHostRequest) (managedssh.HostKeySet, error)
	GetActiveHost(context.Context, managedssh.GetHostKeySetRequest) (managedssh.HostKeySet, error)
	GetPendingHost(context.Context, managedssh.GetHostKeySetRequest) (managedssh.HostKeySet, error)
}

type machineRequestVerifier interface {
	VerifyMachineRequest(context.Context, string, []byte, string, string, []byte) (controlplane.MachineRequestClaims, error)
}

// managedSSHCertificate signs the calling CLI session's managed SSH public key for one machine (F5 via certificates).
func managedSSHCertificate(service managedSSHService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		var body struct {
			MachineGeneration uint64 `json:"machine_generation"`
			PublicKey         string `json:"public_key"`
		}
		if !ok || p.Client == nil {
			writeError(w, r, http.StatusUnauthorized, "authentication_required", "CLI authentication is required.")
			return
		}
		if !decodeManagedSSHJSON(w, r, &body) {
			return
		}
		certificate, err := service.IssueCertificate(r.Context(), managedssh.IssueCertificateRequest{UserID: p.User.ID, CLIClientSessionID: p.Client.SessionID, UserMachineID: r.PathValue("machine_id"), MachineGeneration: body.MachineGeneration, PublicKey: body.PublicKey, Now: time.Now().UTC()})
		if managedSSHError(w, r, err) {
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: wire.CertificateDocument{Type: "ssh_user_certificate", Version: 1, MachineID: r.PathValue("machine_id"), MachineGeneration: body.MachineGeneration, OSUser: certificate.OSUser, Certificate: certificate.Certificate, ValidBefore: certificate.ValidBefore.Format(time.RFC3339)}})
	}
}

func managedSSHRotateUserCA(service managedSSHService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cas, err := service.RotateUserCA(r.Context(), r.PathValue("account_id"), time.Now().UTC())
		if errors.Is(err, managedssh.ErrRotationInvalid) {
			writeManagedSSHInvalid(w, r)
			return
		}
		if managedSSHError(w, r, err) {
			return
		}
		type caView struct {
			ID             string `json:"id"`
			State          string `json:"state"`
			PublicKey      string `json:"public_key"`
			StateChangedAt string `json:"state_changed_at"`
		}
		views := make([]caView, len(cas))
		for index, ca := range cas {
			views[index] = caView{ID: ca.ID, State: ca.State, PublicKey: ca.PublicKey, StateChangedAt: ca.StateChangedAt.UTC().Format(time.RFC3339)}
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: views})
	}
}

func managedSSHTargetPut(service managedSSHService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		var body struct {
			MachineGeneration             uint64 `json:"machine_generation"`
			OSUser                        string `json:"os_user"`
			Port                          uint16 `json:"port"`
			ExpectedReconciliationVersion uint64 `json:"expected_reconciliation_version,omitempty"`
		}
		operationID := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if !ok || operationID == "" || !decodeManagedSSHJSON(w, r, &body) {
			return
		}
		var value managedssh.MachineTarget
		var err error
		if body.ExpectedReconciliationVersion == 0 {
			value, err = service.RegisterTarget(r.Context(), managedssh.RegisterTargetRequest{OperationID: operationID, ActorUserID: p.User.ID, UserMachineID: r.PathValue("machine_id"), MachineGeneration: body.MachineGeneration, OSUser: body.OSUser, TargetPort: body.Port, Now: time.Now().UTC()})
		} else {
			if body.OSUser != "" {
				writeManagedSSHInvalid(w, r)
				return
			}
			value, err = service.UpdateTargetPort(r.Context(), managedssh.UpdateTargetPortRequest{OperationID: operationID, ActorUserID: p.User.ID, UserMachineID: r.PathValue("machine_id"), MachineGeneration: body.MachineGeneration, TargetPort: body.Port, ExpectedReconciliationVersion: body.ExpectedReconciliationVersion, Now: time.Now().UTC()})
		}
		if managedSSHError(w, r, err) {
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: managedSSHTargetResponse(value)})
	}
}

func managedSSHTargetGet(service managedSSHService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		generation, err := parsePositiveQueryGeneration(r)
		if !ok || err != nil {
			writeManagedSSHInvalid(w, r)
			return
		}
		value, err := service.GetTarget(r.Context(), managedssh.GetTargetRequest{ActorUserID: p.User.ID, UserMachineID: r.PathValue("machine_id"), MachineGeneration: generation})
		if managedSSHError(w, r, err) {
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: managedSSHTargetResponse(value)})
	}
}

func managedSSHHostKeysObserve(service managedSSHService, identities machineRequestVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, ok := readManagedSSHBody(w, r)
		if !ok {
			return
		}
		proof, err := base64.RawURLEncoding.Strict().DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
		if err != nil {
			writeError(w, r, http.StatusUnauthorized, "authentication_required", "Machine authentication is required.")
			return
		}
		claims, err := identities.VerifyMachineRequest(r.Context(), r.Header.Get("X-Paperboat-Machine-Identity"), proof, r.Method, r.URL.Path, body)
		if err != nil {
			writeMachineProofFailure(w, r, err, "authentication_required", "Machine authentication is required.")
			return
		}
		if claims.MachineID != r.PathValue("machine_id") || claims.InstallationGeneration <= 0 {
			logHTTPRejection(r, http.StatusUnauthorized, "authentication_required", controlplane.ErrHelperProof)
			writeError(w, r, http.StatusUnauthorized, "authentication_required", "Machine authentication is required.")
			return
		}
		var input struct {
			ObservationGeneration uint64   `json:"observation_generation"`
			PublicKeys            []string `json:"public_keys"`
		}
		if !decodeManagedSSHBytes(body, &input) {
			writeManagedSSHInvalid(w, r)
			return
		}
		value, err := service.ObserveHost(r.Context(), managedssh.ObserveHostRequest{OperationID: claims.OperationID, UserID: claims.UserID, UserMachineID: claims.MachineID, MachineGeneration: uint64(claims.InstallationGeneration), ObservationGeneration: input.ObservationGeneration, PublicKeys: input.PublicKeys, Now: time.Now().UTC()})
		if managedSSHError(w, r, err) {
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: managedSSHHostKeySetResponse(value)})
	}
}

func managedSSHUserCAs(service managedSSHService, identities machineRequestVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, ok := readManagedSSHBody(w, r)
		if !ok {
			return
		}
		var input struct{}
		if !decodeManagedSSHBytes(body, &input) {
			writeManagedSSHInvalid(w, r)
			return
		}
		proof, err := base64.RawURLEncoding.Strict().DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
		if err != nil {
			writeError(w, r, http.StatusUnauthorized, "authentication_required", "Machine authentication is required.")
			return
		}
		claims, err := identities.VerifyMachineRequest(r.Context(), r.Header.Get("X-Paperboat-Machine-Identity"), proof, r.Method, r.URL.Path, body)
		if err != nil {
			writeMachineProofFailure(w, r, err, "authentication_required", "Machine authentication is required.")
			return
		}
		if claims.MachineID != r.PathValue("machine_id") || claims.InstallationGeneration <= 0 {
			logHTTPRejection(r, http.StatusUnauthorized, "authentication_required", controlplane.ErrHelperProof)
			writeError(w, r, http.StatusUnauthorized, "authentication_required", "Machine authentication is required.")
			return
		}
		set, err := service.TrustedUserCAs(r.Context(), managedssh.UserCAsRequest{ActorUserID: claims.UserID, UserMachineID: claims.MachineID, MachineGeneration: uint64(claims.InstallationGeneration), Now: time.Now().UTC()})
		if managedSSHError(w, r, err) {
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: wire.UserCAsDocument{Type: "ssh_user_ca_set", Version: 1, MachineID: claims.MachineID, MachineGeneration: uint64(claims.InstallationGeneration), Keys: set.Keys, Principal: set.Principal, RevokedKeyIDs: set.RevokedKeyIDs}})
	}
}

func managedSSHHostKeysGet(service managedSSHService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		generation, err := parsePositiveQueryGeneration(r)
		if !ok || err != nil {
			writeManagedSSHInvalid(w, r)
			return
		}
		request := managedssh.GetHostKeySetRequest{ActorUserID: p.User.ID, UserMachineID: r.PathValue("machine_id"), MachineGeneration: generation}
		var value managedssh.HostKeySet
		if state := r.URL.Query().Get("state"); state == "pending" {
			value, err = service.GetPendingHost(r.Context(), request)
		} else if state == "" || state == "active" {
			value, err = service.GetActiveHost(r.Context(), request)
		} else {
			writeManagedSSHInvalid(w, r)
			return
		}
		if managedSSHError(w, r, err) {
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: managedSSHHostKeySetResponse(value)})
	}
}

func managedSSHHostKeysPromote(service managedSSHService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		var body struct {
			MachineGeneration   uint64 `json:"machine_generation"`
			ExpectedFingerprint string `json:"expected_fingerprint"`
		}
		operationID := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if !ok || operationID == "" || !decodeManagedSSHJSON(w, r, &body) {
			return
		}
		fingerprint, err := decodeSSHFingerprint(body.ExpectedFingerprint)
		if err != nil {
			writeManagedSSHInvalid(w, r)
			return
		}
		value, err := service.PromoteHost(r.Context(), managedssh.PromoteHostRequest{OperationID: operationID, ActorUserID: p.User.ID, UserMachineID: r.PathValue("machine_id"), MachineGeneration: body.MachineGeneration, SetID: r.PathValue("set_id"), ExpectedFingerprint: fingerprint, Now: time.Now().UTC()})
		if managedSSHError(w, r, err) {
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: managedSSHHostKeySetResponse(value)})
	}
}

func managedSSHTargetResponse(value managedssh.MachineTarget) wire.TargetDocument {
	return wire.TargetDocument{Type: "machine_target", Version: 1, MachineID: value.UserMachineID, MachineGeneration: value.MachineGeneration, OSUser: value.OSUser, Port: value.TargetPort, ReconciliationVersion: value.ReconciliationVersion, CreatedAt: value.CreatedAt.UTC().Format(time.RFC3339), UpdatedAt: value.UpdatedAt.UTC().Format(time.RFC3339)}
}

func managedSSHHostKeySetResponse(value managedssh.HostKeySet) wire.HostKeySetDocument {
	keys := make([]string, len(value.Keys))
	for i := range value.Keys {
		keys[i] = value.Keys[i].PublicKey
	}
	result := wire.HostKeySetDocument{Type: "host_key_set", Version: 1, SetID: value.ID, MachineID: value.UserMachineID, MachineGeneration: value.MachineGeneration, ObservationGeneration: value.ObservationGeneration, Fingerprint: encodeSSHFingerprint(value.Fingerprint), Keys: keys, State: value.State, ReconciliationVersion: value.ReconciliationVersion, ObservedAt: value.ObservedAt.UTC().Format(time.RFC3339)}
	if !value.PromotedAt.IsZero() {
		result.PromotedAt = value.PromotedAt.UTC().Format(time.RFC3339)
	}
	return result
}

func encodeSSHFingerprint(value [sha256.Size]byte) string {
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(value[:])
}

func decodeSSHFingerprint(value string) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	if !strings.HasPrefix(value, "SHA256:") {
		return result, errors.New("invalid fingerprint")
	}
	decoded, err := base64.RawStdEncoding.Strict().DecodeString(strings.TrimPrefix(value, "SHA256:"))
	if err != nil || len(decoded) != len(result) {
		return result, errors.New("invalid fingerprint")
	}
	copy(result[:], decoded)
	if encodeSSHFingerprint(result) != value {
		return [sha256.Size]byte{}, errors.New("invalid fingerprint")
	}
	return result, nil
}

func parsePositiveQueryGeneration(r *http.Request) (uint64, error) {
	values := r.URL.Query()["machine_generation"]
	if len(values) != 1 {
		return 0, errors.New("invalid generation")
	}
	value, err := strconv.ParseUint(values[0], 10, 64)
	if err != nil || value == 0 {
		return 0, errors.New("invalid generation")
	}
	return value, nil
}

func decodeManagedSSHJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	body, ok := readManagedSSHBody(w, r)
	if !ok {
		return false
	}
	if !decodeManagedSSHBytes(body, target) {
		writeManagedSSHInvalid(w, r)
		return false
	}
	return true
}

func readManagedSSHBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxManagedSSHRequest+1))
	if err != nil || len(body) > maxManagedSSHRequest {
		writeManagedSSHInvalid(w, r)
		return nil, false
	}
	return body, true
}

func decodeManagedSSHBytes(body []byte, target any) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target) == nil && errors.Is(decoder.Decode(&struct{}{}), io.EOF)
}

func managedSSHError(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	var inUse *managedssh.HostKeyInUseError
	if errors.As(err, &inUse) {
		details := map[string]any{"state_changed": false}
		if inUse.MachineName != "" {
			details["machine_name"] = inUse.MachineName
		}
		writeErrorDetails(w, r, http.StatusConflict, "ssh_host_key_in_use", "This machine's SSH host keys are identical to another machine's.", details)
		return true
	}
	status, code := http.StatusBadRequest, "invalid_request"
	switch {
	case expectedHTTPErrorOnly(err, wire.ErrInvalid):
	case expectedHTTPErrorOnly(err, managedssh.ErrConflict):
		status, code = http.StatusConflict, "operation_conflict"
	case expectedHTTPErrorOnly(err, managedssh.ErrUnavailable):
		status, code = http.StatusNotFound, "not_found"
	default:
		writeError(w, r, http.StatusServiceUnavailable, "managed_ssh_unavailable", "Managed SSH is temporarily unavailable. Retry shortly.", err)
		return true
	}
	writeError(w, r, status, code, "Managed SSH request could not be completed.")
	return true
}

func writeManagedSSHInvalid(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusBadRequest, "invalid_request", "Managed SSH request is invalid.")
}
