package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat/server/internal/previewv1"
)

// userMachineInboxShare shares one Inbox file as a preview: the machine serves the file itself at the
// preview's address (target scheme "file", address the Inbox file id), so the server never carries its
// bytes. It needs both the Inbox authority (404 otherwise, as the listing) and the preview authority
// of POST /v1/previews, and answers like it. The file must be at most maxBytes.
func userMachineInboxShare(machines inboxAuthority, inbox InboxAPI, previews PreviewLeaseAPI, maxBytes int64) http.HandlerFunc {
	type request struct {
		AccessMode string     `json:"access_mode"`
		ExpiresAt  *time.Time `json:"expires_at"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _, err := readPreviewLeaseJSON(r, true)
		if err != nil {
			writePreviewLeaseError(w, r, err)
			return
		}
		key, err := previewtunnelapi.ParseIdempotencyKey(r.Header)
		if err != nil {
			writePreviewLeaseError(w, r, err)
			return
		}
		var body request
		if err := decodePreviewLeaseBody(raw, &body); err != nil {
			writePreviewLeaseError(w, r, err)
			return
		}
		if body.ExpiresAt == nil {
			writePreviewLeaseError(w, r, fmt.Errorf("%w: expires_at is required", previewv1.ErrInvalidInput))
			return
		}
		requestContext, ok := previewTunnelRequestContext(w, r)
		if !ok || !authorizeInbox(w, r, machines, requestContext.Actor.AccountID, true) {
			return
		}
		listRequest := inboxRequest(r, requestContext.Actor.AccountID)
		listRequest.FileID = ""
		listing, err := inbox.List(r.Context(), listRequest)
		if err != nil {
			writeInboxDispatchError(w, r, err)
			return
		}
		fileID := r.PathValue("file_id")
		found := false
		for _, item := range listing.Items {
			if item.ID != fileID {
				continue
			}
			found = true
			if item.Size > maxBytes {
				writeError(w, r, http.StatusRequestEntityTooLarge, "file_too_large", fmt.Sprintf("Files up to %d MB can be shared as a link. Send larger files with pb send.", maxBytes>>20))
				return
			}
		}
		if !found {
			writeError(w, r, http.StatusNotFound, "inbox_file_not_found", "That file is no longer in the machine's Inbox.")
			return
		}
		// The owner session is derived from the Idempotency-Key, so a retry with the same key replays the
		// same create instead of conflicting with it.
		session := sha256.Sum256([]byte(key))
		result, err := previews.Create(r.Context(), requestContext, previewv1.CreateRequest{
			OwnerMachineID: r.PathValue("machine_id"), OwnerSessionID: "console_" + hex.EncodeToString(session[:16]), OwnerSessionKind: "foreground",
			Target: previewv1.Target{Scheme: "file", Address: fileID}, AccessMode: body.AccessMode, ExpiresAt: body.ExpiresAt,
			Domains: []string{}, IdempotencyKey: key,
		})
		if err != nil {
			writePreviewLeaseError(w, r, err)
			return
		}
		writePreviewCreateResult(w, result)
	}
}
