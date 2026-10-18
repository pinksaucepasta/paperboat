package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/inbox"
)

const (
	inboxFilesPath        = "/v1/inbox-files/"
	inboxOperationHeader  = "X-Paperboat-Operation-Id"
	inboxFileNameHeader   = "X-Paperboat-Inbox-File-Name"
	inboxReadCapability   = "inbox.read.v1"
	inboxDeleteCapability = "inbox.delete.v1"
)

type InboxHandlerConfig struct {
	Authorizer   AuthorizerFactory
	MachineID    string
	InboxPath    func() (string, error)
	Capabilities CapabilityGate
}

// NewInboxHandler serves the control plane's live reads and deletes of one Paperboat Inbox file:
// GET /v1/inbox-files/{id} streams the file, DELETE removes it. Each call carries a single-use
// credential bound to this machine, one file and one operation; the file is found by its id in the
// Inbox folder, never by a path the caller supplies.
func NewInboxHandler(config InboxHandlerConfig) (http.Handler, error) {
	config.MachineID = strings.TrimSpace(config.MachineID)
	if config.Authorizer == nil || config.MachineID == "" || config.InboxPath == nil {
		return nil, errors.New("invalid inbox handler configuration")
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		id, ok := strings.CutPrefix(request.URL.Path, inboxFilesPath)
		if !ok || !inbox.ValidFileID(id) {
			rejectInbox(writer, http.StatusNotFound, "inbox_file_not_found", "path")
			return
		}
		capability, class := inboxReadCapability, "inbox_read"
		switch request.Method {
		case http.MethodGet:
		case http.MethodDelete:
			capability, class = inboxDeleteCapability, "inbox_delete"
		default:
			rejectInbox(writer, http.StatusMethodNotAllowed, "method_not_allowed", "method")
			return
		}
		operationID := request.Header.Get(inboxOperationHeader)
		if len(operationID) < 8 || len(operationID) > 128 || strings.Trim(operationID, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
			rejectInbox(writer, http.StatusBadRequest, "invalid_request", "operation")
			return
		}
		token, ok := bearerToken(request.Header.Values("Authorization"))
		if !ok {
			rejectInbox(writer, http.StatusUnauthorized, "unauthenticated", "bearer")
			return
		}
		authorizer, err := config.Authorizer(token)
		if err != nil {
			rejectInbox(writer, http.StatusUnauthorized, "unauthenticated", "authorizer")
			return
		}
		if closer, ok := authorizer.(interface{ CloseAuthorization() }); ok {
			defer closer.CloseAuthorization()
		}
		authorized, err := authorizer.Authorize(request.Context(), protocol.Frame{Type: "request", Version: protocol.ProtocolVersion, OperationID: operationID, Capability: capability})
		if err != nil || authorized.MachineID != config.MachineID {
			rejectInbox(writer, http.StatusForbidden, "forbidden", "authorization")
			return
		}
		claims, ok := authorized.Value.(auth.Claims)
		if !ok || claims.CredentialClass != class || claims.ResourceKind != "inbox_file" || claims.ResourceID != id || claims.OperationID != operationID ||
			claims.AccountID == "" || claims.ActorID == "" || claims.Subject != claims.ActorID || claims.MachineID != config.MachineID || claims.ExpiresAt <= 0 {
			rejectInbox(writer, http.StatusForbidden, "forbidden", "claims")
			return
		}
		if config.Capabilities != nil && !config.Capabilities.Enabled(capability) {
			rejectInbox(writer, http.StatusConflict, "capability_disabled", "capability")
			return
		}
		root, err := config.InboxPath()
		if err != nil || root == "" {
			rejectInbox(writer, http.StatusServiceUnavailable, "inbox_unavailable", "path")
			return
		}
		if request.Method == http.MethodDelete {
			serveInboxDelete(writer, root, id)
			return
		}
		serveInboxRead(request.Context(), writer, root, id)
	}), nil
}

func serveInboxDelete(writer http.ResponseWriter, root, id string) {
	switch err := inbox.Remove(root, id); {
	case err == nil:
		writer.Header().Set("Cache-Control", "no-store")
		writer.WriteHeader(http.StatusNoContent)
	case errors.Is(err, inbox.ErrFileNotFound):
		rejectInbox(writer, http.StatusNotFound, "inbox_file_not_found", "delete")
	default:
		log.Printf("inbox delete failed: %v", err)
		rejectInbox(writer, http.StatusInternalServerError, "inbox_unavailable", "delete")
	}
}

func serveInboxRead(ctx context.Context, writer http.ResponseWriter, root, id string) {
	handle, file, err := inbox.Open(root, id)
	if errors.Is(err, inbox.ErrFileNotFound) {
		rejectInbox(writer, http.StatusNotFound, "inbox_file_not_found", "read")
		return
	}
	if err != nil {
		log.Printf("inbox read failed: %v", err)
		rejectInbox(writer, http.StatusInternalServerError, "inbox_unavailable", "read")
		return
	}
	defer handle.Close()
	header := writer.Header()
	header.Set("Content-Type", "application/octet-stream")
	header.Set("Content-Length", strconv.FormatInt(file.Size, 10))
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set(inboxFileNameHeader, url.PathEscape(file.Name))
	writer.WriteHeader(http.StatusOK)
	// Exactly the announced size: a file that grows while it is read is cut off, not extended.
	if _, err := io.Copy(writer, io.LimitReader(&contextReader{ctx: ctx, reader: handle}, file.Size)); err != nil {
		log.Printf("inbox read stopped: %v", err)
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func rejectInbox(writer http.ResponseWriter, status int, code, stage string) {
	log.Printf("inbox request rejected stage=%s status=%d", stage, status)
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"error": map[string]any{"code": code}})
}
