package preview

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/httptransport"
)

const maxPreviewReadinessResponseBytes = 64 << 10

// PreviewPublicationTimeout bounds initial managed publication, not lease lifetime.
const PreviewPublicationTimeout = 2 * time.Minute

// HTTPDispatchReadinessObserver reports authenticated edge and origin
// readiness for a server-dispatched preview. The renewable machine identity
// remains in its source and is never copied into the lease or dispatch record.
type HTTPDispatchReadinessObserver struct {
	base       *url.URL
	identities TokenSource
	proofs     ProofSource
	client     *http.Client
}

type HTTPDispatchReadinessObserverConfig struct {
	ControlURL   string
	AllowedHosts []string
	Identities   TokenSource
	Proofs       ProofSource
	Transport    http.RoundTripper
	Timeout      time.Duration
}

func NewHTTPDispatchReadinessObserver(config HTTPDispatchReadinessObserverConfig) (*HTTPDispatchReadinessObserver, error) {
	if config.Timeout == 0 {
		config.Timeout = 15 * time.Second
	}
	base, err := url.Parse(config.ControlURL)
	if err != nil || base.Scheme != "https" || base.User != nil || base.Hostname() == "" || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") || config.Identities == nil || config.Proofs == nil {
		return nil, ErrDispatchInvalid
	}
	if config.Timeout <= 0 || config.Timeout > 30*time.Second {
		return nil, ErrDispatchInvalid
	}
	allowed := false
	for _, host := range config.AllowedHosts {
		if strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(host), "."), strings.TrimSuffix(base.Hostname(), ".")) {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, ErrDispatchInvalid
	}
	transport := config.Transport
	if transport == nil {
		transport = httptransport.Default()
	}
	return &HTTPDispatchReadinessObserver{
		base: base, identities: config.Identities, proofs: config.Proofs,
		client: &http.Client{Transport: transport, Timeout: config.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrDispatchInvalid }},
	}, nil
}

func (o *HTTPDispatchReadinessObserver) ObservePreviewReadiness(ctx context.Context, metadata DispatchReadiness, lease Lease, expectedGeneration int64) (Lease, error) {
	if ctx == nil {
		return Lease{}, ErrDispatchInvalid
	}
	// This bounds initial publication only, never the admitted carrier lifetime.
	ctx, cancel := context.WithTimeout(ctx, PreviewPublicationTimeout)
	defer cancel()
	method := http.MethodPost
	current, err := o.observeLease(ctx, metadata, lease, expectedGeneration, method)
	for err == nil {
		if err := readinessProjectionFailure(method, 0, lease, current, expectedGeneration); err != nil {
			return Lease{}, err
		}
		if current.Generation <= expectedGeneration {
			return Lease{}, readinessFailed(method, "stale_generation", 0, ErrDispatchInvalid, nil)
		}
		if isReadyLease(current) {
			return current, nil
		}
		if err = waitReconnect(ctx, PreviewLeaseDefaultReconnect); err != nil {
			break
		}
		previous := current.Generation
		method = http.MethodGet
		current, err = o.observeLease(ctx, metadata, current, previous, method)
		if err == nil && current.Generation < previous {
			return Lease{}, readinessFailed(method, "stale_generation", 0, ErrDispatchInvalid, nil)
		}
	}
	return Lease{}, err
}

// CurrentLease reads the server's present projection of a lease this machine
// owns, with its current ETag. A lease that is no longer active is an error.
func (o *HTTPDispatchReadinessObserver) CurrentLease(ctx context.Context, stored Lease) (Lease, error) {
	operation, err := newDispatchOperationID()
	if err != nil {
		return Lease{}, err
	}
	trace := "request_" + strings.TrimPrefix(operation, "operation_")
	metadata := DispatchReadiness{OperationID: operation, IdempotencyKey: operation, RequestID: trace, CorrelationID: trace}
	return o.readinessRequest(ctx, metadata, stored, stored.Generation, http.MethodGet, "ready")
}

// ObservePreviewOrigin posts the owner's origin state for a ready preview. The server projects a
// down origin as lease state origin_unavailable and back to ready when the app returns.
func (o *HTTPDispatchReadinessObserver) ObservePreviewOrigin(ctx context.Context, metadata DispatchReadiness, lease Lease, up bool) (Lease, error) {
	if ctx == nil {
		return Lease{}, ErrDispatchInvalid
	}
	originState := "down"
	if up {
		originState = "ready"
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return o.observeLeaseWithOrigin(ctx, metadata, lease, lease.Generation, http.MethodPost, originState)
}

func (o *HTTPDispatchReadinessObserver) observeLease(ctx context.Context, metadata DispatchReadiness, lease Lease, generation int64, method string) (Lease, error) {
	return o.observeLeaseWithOrigin(ctx, metadata, lease, generation, method, "ready")
}

func (o *HTTPDispatchReadinessObserver) observeLeaseWithOrigin(ctx context.Context, metadata DispatchReadiness, lease Lease, generation int64, method, originState string) (Lease, error) {
	if metadata.Mutate == nil {
		return o.readinessRequest(ctx, metadata, lease, generation, method, originState)
	}
	return metadata.Mutate(func(current Lease) (Lease, error) {
		observed, err := o.readinessRequest(ctx, metadata, current, current.Generation, method, originState)
		if err != nil {
			return Lease{}, err
		}
		if err := readinessProjectionFailure(method, 0, current, observed, current.Generation); err != nil {
			return Lease{}, err
		}
		return observed, nil
	})
}

func (o *HTTPDispatchReadinessObserver) readinessRequest(ctx context.Context, metadata DispatchReadiness, lease Lease, expectedGeneration int64, method, originState string) (Lease, error) {
	if o == nil || ctx == nil || expectedGeneration < 1 || metadata.OperationID == "" || metadata.IdempotencyKey == "" || metadata.RequestID == "" || metadata.CorrelationID == "" || lease.ID == "" || lease.OwnerMachineID == "" || lease.OwnerSessionID == "" || lease.ETag == "" || leaseGenerationForID(lease.ID, lease.ETag) != expectedGeneration {
		return Lease{}, readinessFailed(method, "request", 0, ErrDispatchInvalid, nil)
	}
	body, err := json.Marshal(struct {
		OwnerMachineID  string `json:"owner_machine_id"`
		OwnerSessionID  string `json:"owner_session_id"`
		AllocationState string `json:"allocation_state"`
		EdgeState       string `json:"edge_state"`
		OriginState     string `json:"origin_state"`
	}{lease.OwnerMachineID, lease.OwnerSessionID, "ready", "ready", originState})
	if err != nil {
		return Lease{}, readinessFailed(method, "request", 0, ErrDispatchInvalid, err)
	}
	if method == http.MethodGet {
		body = nil
	}
	path := "/v1/previews/" + url.PathEscape(lease.ID) + "/readiness"
	identity, err := o.identities.Token(ctx)
	if err != nil || identity == "" || len(identity) > 16<<10 {
		return Lease{}, readinessFailed(method, "identity", 0, ErrDispatchUnavailable, err)
	}
	proof, err := o.proofs.Proof(ctx, metadata.OperationID, method, path, body)
	if err != nil || len(proof) == 0 || len(proof) > 16<<10 {
		return Lease{}, readinessFailed(method, "proof", 0, ErrDispatchUnavailable, err)
	}
	endpoint := *o.base
	endpoint.Path = path
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return Lease{}, readinessFailed(method, "request", 0, ErrDispatchInvalid, err)
	}
	request.Header.Set("Authorization", "Bearer "+identity)
	request.Header.Set("X-Paperboat-Machine-Identity", identity)
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	if method == http.MethodPost {
		request.Header.Set("If-Match", lease.ETag)
		request.Header.Set("Idempotency-Key", metadata.OperationID)
	}
	request.Header.Set("Request-Id", metadata.RequestID)
	request.Header.Set("Correlation-Id", metadata.CorrelationID)
	response, err := o.client.Do(request)
	if err != nil {
		return Lease{}, readinessFailed(method, "network", 0, ErrDispatchUnavailable, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxPreviewReadinessResponseBytes+1))
	if err != nil || len(data) > maxPreviewReadinessResponseBytes {
		return Lease{}, readinessFailed(method, "response_body", response.StatusCode, ErrDispatchUnavailable, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusConflict || response.StatusCode == http.StatusPreconditionFailed {
			return Lease{}, readinessFailed(method, "http_status", response.StatusCode, ErrDispatchConflict, nil)
		}
		if response.StatusCode >= 500 {
			return Lease{}, readinessFailed(method, "http_status", response.StatusCode, ErrDispatchUnavailable, nil)
		}
		return Lease{}, readinessFailed(method, "http_status", response.StatusCode, ErrDispatchInvalid, nil)
	}
	if err := rejectPreviewReadinessDuplicateKeys(data); err != nil {
		return Lease{}, readinessFailed(method, "json_decode", response.StatusCode, ErrDispatchInvalid, err)
	}
	var envelope struct {
		Data api.PreviewLease `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return Lease{}, readinessFailed(method, "json_decode", response.StatusCode, ErrDispatchInvalid, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Lease{}, readinessFailed(method, "json_decode", response.StatusCode, ErrDispatchInvalid, err)
	}
	envelope.Data.ETag = strings.TrimSpace(response.Header.Get("ETag"))
	observed := leaseFromAPI(envelope.Data)
	if observed.Generation < 1 {
		return Lease{}, readinessFailed(method, "etag_invalid", response.StatusCode, ErrDispatchInvalid, nil)
	}
	if err := readinessProjectionFailure(method, response.StatusCode, lease, observed, expectedGeneration); err != nil {
		return Lease{}, err
	}
	return observed, nil
}

func rejectPreviewReadinessDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return ErrDispatchInvalid
				}
				if _, exists := seen[key]; exists {
					return ErrDispatchInvalid
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return ErrDispatchInvalid
		}
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return ErrDispatchInvalid
	}
	return nil
}

var _ DispatchReadinessObserver = (*HTTPDispatchReadinessObserver)(nil)
