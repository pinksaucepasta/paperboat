package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

// previewDomainLeaseClient adds requested aliases to the canonical lease
// create payload and carries the server's projected public URL through the
// preview session. The server owns alias IDs, readiness, and endpoint changes.
type previewDomainLeaseClient struct {
	client             *api.Client
	domains            []string
	instructionsWriter io.Writer
}

func (c *previewDomainLeaseClient) Create(ctx context.Context, request preview.LeaseRequest) (preview.Lease, error) {
	if c == nil || c.client == nil {
		return preview.Lease{}, errors.New("preview domain lease client is unavailable")
	}
	lease, err := c.client.CreatePreviewLease(ctx, api.PreviewLeaseCreateRequest{
		OwnerMachineID: request.OwnerMachineID,
		OwnerSessionID: request.OwnerSessionID, OwnerSessionKind: request.OwnerSessionKind,
		Target:     api.PreviewLeaseTarget{Scheme: request.Target.Scheme, Address: request.Target.Address},
		AccessMode: request.AccessMode,
		ExpiresAt:  request.UserDeadline,
		Domains:    append([]string(nil), c.domains...),
	}, request.IdempotencyKey)
	if err != nil {
		return preview.Lease{}, err
	}
	for index := range lease.Domains {
		domain := &lease.Domains[index]
		if domain.State == "ready" && domain.Certificate.State == "ready" {
			continue
		}
		instructions, instructionErr := c.client.GetPreviewDomainDNSInstructions(ctx, lease.ID, domain.ID)
		if instructionErr != nil {
			cause := fmt.Errorf("get DNS instructions for %s: %w", domain.Hostname, instructionErr)
			return preview.Lease{}, c.stopCreatedLeaseAfterError(ctx, lease, cause)
		}
		if instructions.Hostname != domain.Hostname {
			cause := fmt.Errorf("get DNS instructions for %s: %w", domain.Hostname, api.ErrPreviewLeaseInvalid)
			return preview.Lease{}, c.stopCreatedLeaseAfterError(ctx, lease, cause)
		}
		domain.Instructions = &instructions
		if c.instructionsWriter != nil {
			if err := writePreviewDomainStatus(c.instructionsWriter, *domain); err != nil {
				cause := fmt.Errorf("write DNS instructions for %s: %w", domain.Hostname, err)
				return preview.Lease{}, c.stopCreatedLeaseAfterError(ctx, lease, cause)
			}
		}
	}
	return previewLeaseFromAPI(lease), nil
}

func (c *previewDomainLeaseClient) stopCreatedLeaseAfterError(ctx context.Context, lease api.PreviewLease, cause error) error {
	key, err := api.NewPreviewLeaseIdempotencyKey()
	if err != nil {
		return errors.Join(cause, fmt.Errorf("could not create cleanup key for preview lease %s; recover with `pb preview stop %s`: %w", lease.ID, lease.ID, err))
	}
	baseCtx := context.Background()
	if ctx != nil {
		baseCtx = context.WithoutCancel(ctx)
	}
	cleanupCtx, cancel := context.WithTimeout(baseCtx, 2*time.Second)
	defer cancel()
	if _, err := c.client.StopPreviewLease(cleanupCtx, lease, key); err != nil {
		return errors.Join(cause, fmt.Errorf("could not stop preview lease %s after DNS instruction failure; recover with `pb preview stop %s`: %w", lease.ID, lease.ID, err))
	}
	return fmt.Errorf("%w; the preview lease was stopped", cause)
}

func (c *previewDomainLeaseClient) Renew(ctx context.Context, lease preview.Lease, idempotencyKey string) (preview.Lease, error) {
	if c == nil || c.client == nil {
		return preview.Lease{}, errors.New("preview domain lease client is unavailable")
	}
	renewed, err := c.client.RenewPreviewLease(ctx, apiLeaseFromPreview(lease), lease.OwnerSessionID, idempotencyKey)
	if err != nil {
		return preview.Lease{}, err
	}
	return previewLeaseFromAPI(renewed), nil
}

func (c *previewDomainLeaseClient) Stop(ctx context.Context, lease preview.Lease, idempotencyKey string) error {
	if c == nil || c.client == nil {
		return errors.New("preview domain lease client is unavailable")
	}
	_, err := c.client.StopPreviewLease(ctx, apiLeaseFromPreview(lease), idempotencyKey)
	return err
}

func (c *previewDomainLeaseClient) Get(ctx context.Context, previewID string) (preview.Lease, error) {
	if c == nil || c.client == nil {
		return preview.Lease{}, errors.New("preview domain lease client is unavailable")
	}
	lease, err := c.client.GetPreviewLease(ctx, previewID)
	if err != nil {
		return preview.Lease{}, err
	}
	return previewLeaseFromAPI(lease), nil
}

func apiLeaseFromPreview(lease preview.Lease) api.PreviewLease {
	return api.PreviewLease{
		Schema:         lease.Schema,
		Kind:           lease.Kind,
		ID:             lease.ID,
		AccountID:      lease.AccountID,
		ActorID:        lease.ActorID,
		OwnerMachineID: lease.OwnerMachineID,
		OwnerSessionID: lease.OwnerSessionID, OwnerSessionKind: lease.OwnerSessionKind,
		Target:            api.PreviewLeaseTarget{Scheme: lease.Target.Scheme, Address: lease.Target.Address},
		AccessMode:        lease.AccessMode,
		Persistent:        lease.Persistent,
		Endpoint:          lease.Endpoint,
		EndpointState:     lease.EndpointState,
		LeaseDeadline:     lease.LeaseDeadline,
		UserDeadline:      lease.UserDeadline,
		State:             lease.State,
		AllocationState:   lease.AllocationState,
		EdgeState:         lease.EdgeState,
		OriginState:       lease.OriginState,
		CreatedAt:         lease.CreatedAt,
		LastRenewedAt:     lease.LastRenewedAt,
		CreateOperationID: lease.CreateOperationID,
		ETag:              lease.ETag,
	}
}

func previewLeaseFromAPI(lease api.PreviewLease) preview.Lease {
	return preview.Lease{
		Schema:         lease.Schema,
		Kind:           lease.Kind,
		ID:             lease.ID,
		AccountID:      lease.AccountID,
		ActorID:        lease.ActorID,
		OwnerMachineID: lease.OwnerMachineID,
		OwnerSessionID: lease.OwnerSessionID, OwnerSessionKind: lease.OwnerSessionKind,
		Target:            preview.LeaseTarget{Scheme: lease.Target.Scheme, Address: lease.Target.Address},
		AccessMode:        lease.AccessMode,
		Persistent:        lease.Persistent,
		Endpoint:          lease.Endpoint,
		EndpointState:     lease.EndpointState,
		DomainCount:       len(lease.Domains),
		LeaseDeadline:     lease.LeaseDeadline,
		UserDeadline:      lease.UserDeadline,
		State:             lease.State,
		AllocationState:   lease.AllocationState,
		EdgeState:         lease.EdgeState,
		OriginState:       lease.OriginState,
		CreatedAt:         lease.CreatedAt,
		LastRenewedAt:     lease.LastRenewedAt,
		CreateOperationID: lease.CreateOperationID,
		ETag:              lease.ETag,
		Generation:        previewLeaseGeneration(lease.ETag),
	}
}

func previewLeaseGeneration(etag string) int64 {
	value := strings.Trim(etag, `"`)
	parts := strings.Split(value, ":")
	if len(parts) == 4 {
		generation, _ := strconv.ParseInt(parts[3], 10, 64)
		return generation
	}
	return 0
}

func observePreviewDomains(ctx context.Context, client *api.Client, previewID string, requested []string, writer io.Writer) error {
	if writer == nil {
		return errors.New("preview domain writer is required")
	}
	return observePreviewDomainChanges(ctx, client, previewID, requested, func(domain api.PreviewDomainSummary) error { return writePreviewDomainStatus(writer, domain) })
}

func observePreviewDomainChanges(ctx context.Context, client *api.Client, previewID string, requested []string, observe func(api.PreviewDomainSummary) error) error {
	if ctx == nil || client == nil || observe == nil {
		return errors.New("preview domain observer is not configured")
	}
	if len(requested) == 0 {
		return nil
	}
	requested, err := api.NormalizePreviewDomains(requested)
	if err != nil {
		return err
	}
	seen := make(map[string]string, len(requested))
	hadFailure := false
	reportedFailure := false
	var failureContext context.Context
	for {
		lease, err := client.GetPreviewLease(ctx, previewID)
		if err == nil {
			if hadFailure {
				if reporter := errorreport.Current(); reporter != nil {
					reporter.Observe(failureContext, "paperboat-cli", "preview", "success", 0)
				}
				if recorder := diagnostics.FromContext(failureContext); recorder != nil {
					_ = recorder.RecordWithSupportReference("preview", "recovered", "info", supportref.FromContext(failureContext), map[string]string{
						"component": "paperboat-cli",
						"operation": "preview",
					})
				}
			}
			hadFailure = false
			reportedFailure = false
			failureContext = nil
			terminal := true
			ready := true
			for _, hostname := range requested {
				domain, ok := previewDomainByHostname(lease.Domains, hostname)
				if !ok {
					terminal = false
					ready = false
					continue
				}
				key := previewDomainObservationKey(domain)
				if seen[hostname] != key {
					seen[hostname] = key
					if err := observe(domain); err != nil {
						return previewFailure("preview domain status output failed", err)
					}
				}
				if previewDomainReady(domain) {
					continue
				}
				ready = false
				switch domain.State {
				case "released", "expired", "conflict", "dns_error", "tls_error", "quarantined":
				default:
					terminal = false
				}
			}
			if ready || terminal {
				return nil
			}
		} else {
			fault := errorreport.ProjectFault(ctx, "paperboat-cli", "preview", "command", "command_failed", err)
			if fault.Outcome != "canceled" {
				hadFailure = true
				if failureContext == nil {
					failureContext = supportref.WithContext(ctx, fault.SupportReference)
				}
				if !reportedFailure && !errorreport.HTTPAttemptObserved(err) {
					errorreport.Current().ObserveFailure(failureContext, "paperboat-cli", "preview", "command", "command_failed", err)
					reportedFailure = true
				}
			}
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil
		}
	}
}

func previewDomainByHostname(domains []api.PreviewDomainSummary, hostname string) (api.PreviewDomainSummary, bool) {
	for _, domain := range domains {
		if domain.Hostname == hostname {
			return domain, true
		}
	}
	return api.PreviewDomainSummary{}, false
}

func previewDomainReady(domain api.PreviewDomainSummary) bool {
	return domain.State == "ready" && domain.Certificate.State == "ready"
}

func previewDomainObservationKey(domain api.PreviewDomainSummary) string {
	data, _ := json.Marshal(struct {
		State        string                       `json:"state"`
		DNS          api.PreviewDomainDNS         `json:"dns"`
		Certificate  api.PreviewDomainCertificate `json:"certificate"`
		Instructions *api.PreviewDNSInstructions  `json:"instructions,omitempty"`
	}{domain.State, domain.DNS, domain.Certificate, domain.Instructions})
	return string(data)
}

func writePreviewDomainJSON(writer io.Writer, previewID string, domain api.PreviewDomainSummary) error {
	return writeCLIJSON(writer, struct {
		Event     string                   `json:"event"`
		PreviewID string                   `json:"preview_id"`
		Domain    api.PreviewDomainSummary `json:"domain"`
	}{"domain_status", previewID, domain})
}

func writePreviewDomainStatus(writer io.Writer, domain api.PreviewDomainSummary) error {
	var output strings.Builder
	_, _ = fmt.Fprintf(&output, "Domain %s: state=%s certificate=%s\n", domain.Hostname, domain.State, domain.Certificate.State)
	if domain.Instructions == nil {
		_, err := io.WriteString(writer, output.String())
		return err
	}
	_, _ = fmt.Fprintf(&output, "DNS instructions for %s (%s):\n", domain.Hostname, domain.Instructions.VerificationState)
	for _, record := range domain.Instructions.Records {
		_, _ = fmt.Fprintf(&output, "  %s %s -> %s (TTL %d)\n", record.Type, record.Name, record.Value, record.TTL)
	}
	if domain.Instructions.Note != "" {
		_, _ = fmt.Fprintf(&output, "  %s\n", domain.Instructions.Note)
	}
	_, err := io.WriteString(writer, output.String())
	return err
}
