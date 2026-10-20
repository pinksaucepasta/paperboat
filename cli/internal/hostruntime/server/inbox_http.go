package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/inbox"
)

const (
	inboxFilesPath        = "/v1/inbox-files/"
	inboxListPath         = "/v1/inbox-files"
	inboxListLimit        = 200
	inboxOperationHeader  = "X-Paperboat-Operation-Id"
	inboxReadCapability   = "inbox.read.v1"
	inboxDeleteCapability = "inbox.delete.v1"
	inboxListCapability   = "inbox.list.v1"
)

type InboxHandlerConfig struct {
	Authorizer   AuthorizerFactory
	MachineID    string
	InboxPath    func() (string, error)
	Capabilities CapabilityGate
}

// NewInboxHandler serves the control plane's live view of the Paperboat Inbox: GET /v1/inbox-files
// lists it, GET /v1/inbox-files/{id} streams one file, DELETE removes it. Each call carries a
// single-use credential bound to this machine and one operation (and one file); the file is found
// by its id in the Inbox folder, never by a path the caller supplies. Nothing is kept anywhere else.
func NewInboxHandler(config InboxHandlerConfig) (http.Handler, error) {
	config.MachineID = strings.TrimSpace(config.MachineID)
	if config.Authorizer == nil || config.MachineID == "" || config.InboxPath == nil {
		return nil, errors.New("invalid inbox handler configuration")
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		listing := request.URL.Path == inboxListPath
		id, ok := strings.CutPrefix(request.URL.Path, inboxFilesPath)
		if !listing && (!ok || !inbox.ValidFileID(id)) {
			rejectInbox(writer, http.StatusNotFound, "inbox_file_not_found", "path")
			return
		}
		capability, class := inboxReadCapability, "inbox_read"
		switch {
		case request.Method == http.MethodGet && listing:
			capability, class = inboxListCapability, "inbox_list"
		case request.Method == http.MethodGet:
		case request.Method == http.MethodDelete && !listing:
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
		wantKind, wantResource := "inbox_file", id
		if listing {
			wantKind, wantResource = "inbox", config.MachineID
		}
		if !ok || claims.CredentialClass != class || claims.ResourceKind != wantKind || claims.ResourceID != wantResource || claims.OperationID != operationID ||
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
		if listing {
			serveInboxList(writer, root)
			return
		}
		if request.Method == http.MethodDelete {
			serveInboxDelete(writer, root, id)
			return
		}
		serveInboxRead(request.Context(), writer, root, id)
	}), nil
}

func serveInboxList(writer http.ResponseWriter, root string) {
	files, err := inbox.Files(root)
	if err != nil {
		log.Printf("inbox list failed: %v", err)
		rejectInbox(writer, http.StatusServiceUnavailable, "inbox_unavailable", "list")
		return
	}
	type item struct {
		ID         string    `json:"id"`
		Name       string    `json:"name"`
		Size       int64     `json:"size"`
		ReceivedAt time.Time `json:"received_at"`
	}
	answer := struct {
		Total int    `json:"total"`
		Items []item `json:"items"`
	}{Total: len(files), Items: make([]item, 0, min(len(files), inboxListLimit))}
	for _, file := range files {
		if len(answer.Items) == inboxListLimit {
			break
		}
		answer.Items = append(answer.Items, item{ID: file.ID, Name: file.Name, Size: file.Size, ReceivedAt: file.ReceivedAt})
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(writer).Encode(answer)
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
	// The file name travels in the standard header: the edge removes X-Paperboat-* headers from every
	// response, so a private header never reached the control plane.
	header.Set("Content-Disposition", "attachment; filename*=UTF-8''"+inboxDispositionName(file.Name))
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

// inboxDispositionName percent-encodes a file name for an RFC 5987 extended filename parameter.
func inboxDispositionName(name string) string {
	const unreserved = "!#$&+-.^_`|~"
	var encoded strings.Builder
	for _, b := range []byte(name) {
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9', strings.IndexByte(unreserved, b) >= 0:
			encoded.WriteByte(b)
		default:
			fmt.Fprintf(&encoded, "%%%02X", b)
		}
	}
	return encoded.String()
}
