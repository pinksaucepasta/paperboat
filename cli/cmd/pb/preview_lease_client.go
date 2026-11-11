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
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

// previewDomainLeaseClient adds requested aliases to the canonical lease
// create payload and carries the server's projected public URL through the
// preview session. The server owns alias IDs, readiness, and endpoint changes.
type previewDomainLeaseClient struct {
	client  *api.Client
	domains []string
}

func (c *previewDomainLeaseClient) Create(ctx context.Context, request preview.LeaseRequest) (preview.Lease, error) {
	if c == nil || c.client == nil {
		return preview.Lease{}, errors.New("preview domain lease client is unavailable")
	}
	lease, err := c.client.CreatePreviewLease(ctx, api.PreviewLeaseCreateRequest{
		OwnerMachineID: request.OwnerMachineID,
		OwnerSessionID: request.OwnerSessionID, OwnerSessionKind: request.OwnerSessionKind,
		Target:     api.PreviewLeaseTarget{Scheme: request.Target.Scheme, Address: request.Target.Address, HostHeader: request.Target.HostHeader},
		AccessMode: request.AccessMode,
		ExpiresAt:  request.UserDeadline,
		Domains:    append([]string(nil), c.domains...),
	}, request.IdempotencyKey)
	if err != nil {
		return preview.Lease{}, err
	}
	return previewLeaseFromAPI(lease), nil
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

// WaitMachineEvents lets the foreground observer wait for the preview's owner machine topics instead
// of polling the lease.
func (c *previewDomainLeaseClient) WaitMachineEvents(ctx context.Context, machineID string, known map[string]int64) (map[string]int64, error) {
	if c == nil || c.client == nil {
		return nil, errors.New("preview domain lease client is unavailable")
	}
	return c.client.WaitMachineEvents(ctx, machineID, known)
}

var _ preview.LeaseChangeWaiter = (*previewDomainLeaseClient)(nil)

func apiLeaseFromPreview(lease preview.Lease) api.PreviewLease {
	return api.PreviewLease{
		Schema:         lease.Schema,
		Kind:           lease.Kind,
		ID:             lease.ID,
		AccountID:      lease.AccountID,
		ActorID:        lease.ActorID,
		OwnerMachineID: lease.OwnerMachineID,
		OwnerSessionID: lease.OwnerSessionID, OwnerSessionKind: lease.OwnerSessionKind,
		Target:            api.PreviewLeaseTarget{Scheme: lease.Target.Scheme, Address: lease.Target.Address, HostHeader: lease.Target.HostHeader},
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
		Target:            preview.LeaseTarget{Scheme: lease.Target.Scheme, Address: lease.Target.Address, HostHeader: lease.Target.HostHeader},
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
	// Domain progress does not move an event topic, so a domain that waits on the user's DNS backs off
	// to one read per previewDomainPollMax; any observed change reads at the fast interval again.
	delay := previewDomainPollMin
	for {
		changed := false
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
					changed = true
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
		if changed {
			delay = previewDomainPollMin
		}
		timer := time.NewTimer(delay)
		delay = min(delay*2, previewDomainPollMax)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil
		}
	}
}

const (
	previewDomainPollMin = 500 * time.Millisecond
	previewDomainPollMax = 15 * time.Second
)

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
		State       string                       `json:"state"`
		DNS         api.PreviewDomainDNS         `json:"dns"`
		Certificate api.PreviewDomainCertificate `json:"certificate"`
	}{domain.State, domain.DNS, domain.Certificate})
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
	_, err := fmt.Fprintf(writer, "Domain %s: state=%s certificate=%s\n", domain.Hostname, domain.State, domain.Certificate.State)
	return err
}
