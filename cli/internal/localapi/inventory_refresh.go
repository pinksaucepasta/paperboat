package localapi

import (
	"context"
	"io"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"net/http"
)

type inventoryRefresher interface{ Refresh(context.Context) error }

// inventoryRefreshTimeout bounds one requested refresh. The refresh runs its stages (machine list,
// local reconcile, completion items) with a budget each, so it can legitimately take longer than the
// ordinary request timeout; a refresh the caller asked for must not be cut off by that timeout (a
// sign-in waits for it).
const inventoryRefreshTimeout = 45 * time.Second

func (s *Server) refreshInventory(w http.ResponseWriter, r *http.Request, requestID string, peer Peer) {
	if !s.ownsPeer(peer) {
		writeError(w, http.StatusForbidden, requestID, "permission_denied", "inventory refresh requires the installed owner")
		return
	}
	if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.ContentLength != 0 || r.Header.Get("Content-Type") != "" {
		writeError(w, http.StatusBadRequest, requestID, "invalid_request", "inventory refresh requires an empty POST")
		return
	}
	source, ok := s.config.Source.(inventoryRefresher)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, requestID, "inventory_unavailable", "inventory refresh is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), inventoryRefreshTimeout)
	defer cancel()
	if err := source.Refresh(ctx); err != nil {
		errorreport.Current().CaptureFailure(ctx, "paperboatd", "login", "local_gateway", "local_gateway_failed", err)
		writeError(w, http.StatusServiceUnavailable, requestID, "inventory_unavailable", "inventory refresh failed; run pb doctor")
		return
	}
	w.Header().Set("X-Paperboat-Protocol", ProtocolV1)
	w.WriteHeader(http.StatusNoContent)
}

// RefreshInventory waits for the owner daemon to publish the current account's inventory.
func (c *Client) RefreshInventory(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://paperboat.local/v1/inventory/refresh", nil)
	if err != nil {
		return err
	}
	request.Header.Set("X-Paperboat-Request-ID", localRequestID())
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.Header.Get("X-Paperboat-Protocol") != ProtocolV1 {
		return ErrVersionMismatch
	}
	if response.StatusCode != http.StatusNoContent {
		return decodeRemoteErrorReader(response.StatusCode, io.LimitReader(response.Body, maxJSONBytes+1))
	}
	return nil
}
