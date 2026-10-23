package previewv1

import (
	"context"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
	"strings"
)

// GetMachineReadiness is an exact owner read, including terminal states so the
// publisher can finish cleanup. No user management or unrelated machine grant
// can substitute for the machine identity that owns this preview.
func (s *Service) GetMachineReadiness(ctx context.Context, request previewtunnelapi.RequestContext, preview string) (PreviewResult, error) {
	if request.Actor.MachineID == "" || strings.TrimSpace(preview) == "" {
		return PreviewResult{}, ErrOwnerDenied
	}
	if err := s.authorize(request, "read"); err != nil {
		return PreviewResult{}, err
	}
	request, err := s.machineLeaseRequest(ctx, request, preview)
	if err != nil {
		return PreviewResult{}, err
	}
	lease, err := s.repository.GetPreviewLeaseV1(ctx, request.Actor.AccountID, preview)
	if err != nil {
		return PreviewResult{}, err
	}
	if lease.OwnerMachineID != request.Actor.MachineID {
		return PreviewResult{}, ErrOwnerDenied
	}
	view, err := s.previewView(ctx, lease)
	if err != nil {
		return PreviewResult{}, err
	}
	return PreviewResult{Preview: view, ETag: previewtunnelapi.ETag(Kind, lease.ID, lease.Generation)}, nil
}
