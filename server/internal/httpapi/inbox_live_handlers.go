package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/inboxdispatch"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// InboxAPI is the live path to a machine's Inbox: the machine answers, the server stores no bytes.
type InboxAPI interface {
	List(context.Context, inboxdispatch.Request) (inboxdispatch.Listing, error)
	Open(context.Context, inboxdispatch.Request) (inboxdispatch.File, error)
	Delete(context.Context, inboxdispatch.Request) error
}

// inboxAuthority says whether an actor may manage a machine's Inbox (*usermachines.Service).
type inboxAuthority interface {
	CanManageInbox(context.Context, string, string) error
}

// inboxIdleTimeout ends a download whose machine stops sending.
const inboxIdleTimeout = 30 * time.Second

var inboxFileID = regexp.MustCompile(`^[0-9a-f]{16}$`)

func inboxRequest(r *http.Request, principalID string) inboxdispatch.Request {
	request := inboxdispatch.Request{
		ActorID: principalID, MachineID: r.PathValue("machine_id"), FileID: r.PathValue("file_id"),
		OperationID: "inbox-" + uuid.NewString(), RequestID: requestIDFromContext(r.Context()), CorrelationID: supportReference(r.Context()),
	}
	if scope, ok := workspace.FromContext(r.Context()); ok && scope.TeamID != "" {
		request.TeamID = sql.NullString{String: scope.TeamID, Valid: true}
	}
	return request
}

// authorizeInbox answers 404 for machines the actor may not manage, as the listing does: file names
// and machines are private.
func authorizeInbox(w http.ResponseWriter, r *http.Request, machines inboxAuthority, userID string, file bool) bool {
	if file && !inboxFileID.MatchString(r.PathValue("file_id")) {
		writeError(w, r, http.StatusNotFound, "inbox_file_not_found", "That file is not in the machine's Inbox.")
		return false
	}
	if err := machines.CanManageInbox(r.Context(), userID, r.PathValue("machine_id")); err != nil {
		if errors.Is(err, usermachines.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "user_machine_not_found", "Machine was not found.")
		} else {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Unable to read the machine inbox.", err)
		}
		return false
	}
	return true
}

func writeInboxDispatchError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, inboxdispatch.ErrFileNotFound):
		writeError(w, r, http.StatusNotFound, "inbox_file_not_found", "That file is no longer in the machine's Inbox.")
	case errors.Is(err, inboxdispatch.ErrMachineOutdated):
		writeError(w, r, http.StatusConflict, "machine_update_required", "This machine runs an older Paperboat that cannot list its Inbox. Update Paperboat on it, then try again.")
	case errors.Is(err, inboxdispatch.ErrMachineOffline):
		writeError(w, r, http.StatusConflict, "machine_offline", "The machine is offline or is not receiving files. Its Inbox can be read again when it is back.")
	case errors.Is(err, inboxdispatch.ErrCapabilityDisabled):
		writeError(w, r, http.StatusConflict, "inbox_unavailable", "The machine has turned off receiving files.")
	case errors.Is(err, inboxdispatch.ErrFileTooLarge):
		writeError(w, r, http.StatusRequestEntityTooLarge, "file_too_large", "This file is too large to download here. Use pb on the machine or a transfer with pb send.")
	case errors.Is(err, inboxdispatch.ErrMachineUnreachable):
		writeError(w, r, http.StatusServiceUnavailable, "machine_unreachable", "The machine did not answer. Try again in a moment.", err)
	case errors.Is(err, inboxdispatch.ErrInvalidConfiguration):
		writeError(w, r, http.StatusBadRequest, "invalid_request", "The request does not name one Inbox file.")
	default:
		writeError(w, r, http.StatusInternalServerError, "internal_error", "The Inbox request failed.", err)
	}
}

// userMachineInboxList lists a machine's Inbox as the machine sees it right now; the server keeps
// nothing. An offline machine answers 409 machine_offline.
func userMachineInboxList(machines inboxAuthority, inbox InboxAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		if !authorizeInbox(w, r, machines, principal.User.ID, false) {
			return
		}
		request := inboxRequest(r, principal.User.ID)
		request.FileID = ""
		listing, err := inbox.List(r.Context(), request)
		if err != nil {
			writeInboxDispatchError(w, r, err)
			return
		}
		noStore(w)
		writeJSON(w, http.StatusOK, SuccessResponse{Data: listing})
	}
}

// userMachineInboxContent streams one Inbox file from the machine to the browser.
func userMachineInboxContent(machines inboxAuthority, inbox InboxAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		if !authorizeInbox(w, r, machines, principal.User.ID, true) {
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		file, err := inbox.Open(ctx, inboxRequest(r.WithContext(ctx), principal.User.ID))
		if err != nil {
			writeInboxDispatchError(w, r, err)
			return
		}
		defer file.Body.Close()
		name := file.Name
		if name == "" {
			name = "download"
		}
		header := w.Header()
		header.Set("Content-Type", "application/octet-stream")
		header.Set("Content-Length", strconv.FormatInt(file.Size, 10))
		header.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
		header.Set("X-Content-Type-Options", "nosniff")
		noStore(w)
		w.WriteHeader(http.StatusOK)
		// A machine that stops sending ends the download instead of holding the connection.
		idle := time.AfterFunc(inboxIdleTimeout, cancel)
		defer idle.Stop()
		_, _ = io.Copy(w, &idleReader{reader: file.Body, timer: idle, timeout: inboxIdleTimeout})
	}
}

type idleReader struct {
	reader  io.Reader
	timer   *time.Timer
	timeout time.Duration
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.timer.Reset(r.timeout)
	}
	return n, err
}

// userMachineInboxDelete removes one Inbox file on the machine. The caller's Idempotency-Key is
// bound into the machine credential.
func userMachineInboxDelete(machines inboxAuthority, inbox InboxAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" || len(key) > 256 || strings.ContainsAny(key, " \t\r\n") {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "An Idempotency-Key header is required.")
			return
		}
		if !authorizeInbox(w, r, machines, principal.User.ID, true) {
			return
		}
		request := inboxRequest(r, principal.User.ID)
		request.IdempotencyKey = key
		if err := inbox.Delete(r.Context(), request); err != nil {
			writeInboxDispatchError(w, r, err)
			return
		}
		noStore(w)
		w.WriteHeader(http.StatusNoContent)
	}
}
