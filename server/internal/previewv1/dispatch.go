package previewv1

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelstore"
	"github.com/pinksaucepasta/paperboat/shared/previewdispatch"
)

const PreviewDispatchKind = "preview_dispatch"

var (
	ErrDispatchInvalid     = errors.New("invalid preview dispatch")
	ErrDispatchMismatch    = errors.New("preview dispatch response mismatch")
	ErrDispatchUnavailable = errors.New("preview dispatch unavailable")
	ErrDispatchUncertain   = errors.New("preview dispatch outcome is uncertain")
	ErrDispatchRejected    = errors.New("preview dispatch rejected")
)

// LazyBinding pins activation to the current policy, installation and runtime boot.
type LazyBinding = previewdispatch.LazyBinding

// DispatchRequest is the secret-free, operation-bound lease projection sent to the selected host. The wire
// document and its canonical hash input are shared with the host; RequestHash excludes itself and covers every
// other field, including LeaseETag.
type DispatchRequest struct {
	previewdispatch.Request
}

// DispatchOutcome is the only response data accepted from a host. A host may accept before readiness; the
// server's machine-auth readiness observation is the authority that completes the create operation.
type DispatchOutcome = previewdispatch.Outcome

// Dispatcher is deliberately narrow so the preview policy service can be
// tested without an HTTP server or database. Implementations must perform
// network I/O only after the lease/operation transaction has committed.
type Dispatcher interface {
	Dispatch(context.Context, DispatchRequest) (DispatchOutcome, error)
}

// ComputeRequestHash returns the lowercase SHA-256 hex digest used by the
// host. The request_hash member itself is intentionally not included.
func (r DispatchRequest) ComputeRequestHash() (string, error) {
	canonical, err := r.canonicalHashInput(r.OwnerMachineID)
	if err != nil {
		return "", err
	}
	return previewdispatch.HashOf(canonical), nil
}

// Validate checks identity, lifecycle, generation, and exact request hash.
// now is optional for deterministic construction; a non-zero value also
// rejects expired lease/user deadlines.
func (r DispatchRequest) Validate(now time.Time) error {
	canonical, err := r.canonicalHashInput(r.OwnerMachineID)
	if err != nil {
		return err
	}
	if r.RequestHash != previewdispatch.HashOf(canonical) {
		return fmt.Errorf("%w: request hash mismatch", ErrDispatchInvalid)
	}
	if !now.IsZero() {
		when := now.UTC()
		if !r.LeaseDeadline.After(when) || r.UserDeadline != nil && !r.UserDeadline.After(when) {
			return fmt.Errorf("%w: lease deadline has passed", ErrDispatchInvalid)
		}
	}
	return nil
}

func (r DispatchRequest) canonicalHashInput(machineID string) ([]byte, error) {
	r.Request = previewdispatch.Normalize(r.Request)
	if (r.OwnerSessionKind == "lazy_runtime") != (r.Lazy != nil) {
		return nil, ErrDispatchInvalid
	}
	if r.Lazy != nil && (!validDispatchID(r.Lazy.PolicyID) || r.Lazy.PolicyGeneration < 1 || r.Lazy.InstallationGeneration < 1 || !validLazyBoot(r.Lazy.BootID) || r.AccessMode == "public" || r.OwnerSessionKind != "lazy_runtime") {
		return nil, ErrDispatchInvalid
	}
	if r.Schema != Schema || r.Kind != PreviewDispatchKind || !validDispatchID(r.PreviewID) || !validDispatchID(r.OperationID) || !validDispatchID(r.AccountID) || !validDispatchID(r.ActorID) || !validDispatchID(r.OwnerMachineID) || r.OwnerMachineID != strings.TrimSpace(machineID) || !validDispatchID(r.OwnerSessionID) || !previewtunnelstore.ValidPreviewOwnerSessionKind(r.OwnerSessionKind) || r.ExpectedGeneration < 1 {
		return nil, ErrDispatchInvalid
	}
	if !validDispatchTrace(r.IdempotencyKey, 1, 256) || !validDispatchTrace(r.RequestID, 3, 128) || !validDispatchTrace(r.CorrelationID, 3, 128) {
		return nil, ErrDispatchInvalid
	}
	if !previewtunnelstore.ValidPreviewTargetV1(r.Target.Scheme, r.Target.Address, r.AccessMode) || !previewtunnelstore.ValidPreviewHostHeaderV1(r.Target.Scheme, r.Target.HostHeader) {
		return nil, ErrDispatchInvalid
	}
	if r.AccessMode != "public" && r.AccessMode != "private" && r.AccessMode != "team" || !validDispatchEndpoint(r.Endpoint) {
		return nil, ErrDispatchInvalid
	}
	if r.LeaseDeadline.IsZero() || r.UserDeadline != nil && r.UserDeadline.IsZero() || r.CreatedAt.IsZero() || r.LastRenewedAt.IsZero() || r.LastRenewedAt.Before(r.CreatedAt) {
		return nil, ErrDispatchInvalid
	}
	if !validDispatchState(r.State, r.AllocationState, r.EdgeState, r.OriginState) || !validDispatchETag(r.LeaseETag, r.PreviewID, r.ExpectedGeneration) {
		return nil, ErrDispatchInvalid
	}
	return previewdispatch.MarshalHashInput(r.Request)
}

func validDispatchID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) < 3 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f || r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return false
		}
	}
	return true
}

func validDispatchTrace(value string, minimum, maximum int) bool {
	if len(value) < minimum || len(value) > maximum {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f || r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return false
		}
	}
	return true
}

func validDispatchEndpoint(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" && parsed.Path != "/" || parsed.RawPath != "" && parsed.RawPath != "/" || parsed.Host == "" || parsed.Port() != "" {
		return false
	}
	labels := strings.Split(parsed.Hostname(), ".")
	if len(labels) < 2 || strings.HasPrefix(labels[0], "preview-") {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if r != '-' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
				return false
			}
		}
	}
	return true
}

func validDispatchETag(raw, id string, expected int64) bool {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return false
	}
	parts := strings.Split(strings.Trim(raw, `"`), ":")
	if len(parts) != 4 || parts[0] != "ptv1" || parts[1] != Kind {
		return false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil || string(decoded) != id || base64.RawURLEncoding.EncodeToString(decoded) != parts[2] || strconv.FormatInt(expected, 10) != parts[3] {
		return false
	}
	return expected > 0
}

func validDispatchState(state, allocation, edge, origin string) bool {
	return (state == "allocating" || state == "connecting" || state == "ready" || state == "origin_unavailable") &&
		(allocation == "pending" || allocation == "ready" || allocation == "failed" || allocation == "released") &&
		(edge == "pending" || edge == "ready" || edge == "degraded" || edge == "down") &&
		(origin == "unknown" || origin == "ready" || origin == "unavailable")
}

func validLazyBoot(value string) bool {
	if len(value) < 16 || len(value) > 64 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
