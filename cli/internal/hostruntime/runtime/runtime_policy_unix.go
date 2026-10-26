//go:build darwin || linux || windows

package runtime

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
)

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
