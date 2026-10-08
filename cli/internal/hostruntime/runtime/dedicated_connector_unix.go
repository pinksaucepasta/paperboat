//go:build darwin || linux || windows

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
)

// The tunnel manager owns connector lifetimes. This component owns only the
// platform network monitor; status comes from already-authorized active carriers.
type dedicatedConnectorService struct {
	networkChanges Service
	status         func() connector.Status
}

func (s *dedicatedConnectorService) Start(ctx context.Context) error {
	return s.networkChanges.Start(ctx)
}
func (s *dedicatedConnectorService) Shutdown(ctx context.Context) error {
	return s.networkChanges.Shutdown(ctx)
}
func (s *dedicatedConnectorService) Status() connector.Status {
	if s.status == nil {
		return connector.Status{}
	}
	return s.status()
}

func applyRuntimeTransferPolicy(body []byte, policy *filetransfer.PolicyStore) error {
	var response struct {
		Data struct {
			Policy *filetransfer.Policy `json:"file_transfer_policy"`
		} `json:"data"`
	}
	if policy == nil || json.Unmarshal(body, &response) != nil || response.Data.Policy == nil {
		return errors.New("runtime response has no valid file transfer policy")
	}
	return policy.Update(*response.Data.Policy)
}

func rejectRuntimePolicyRedirect(*http.Request, []*http.Request) error { return ErrProductionInvalid }
