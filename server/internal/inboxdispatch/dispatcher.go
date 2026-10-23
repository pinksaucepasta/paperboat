// Package inboxdispatch reads and deletes one Paperboat Inbox file on a machine, live, for the
// console. It reuses the control plane's server-to-daemon HTTP boundary (the machine's runtime
// ingress, as preview dispatch does) with a short single-use credential bound to the machine, the
// file and the operation. The server stores no file bytes: a read is streamed through.
package inboxdispatch

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/admission"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/runtimeattachment"
)

const (
	// DefaultHeaderTimeout bounds how long the machine may take to answer before the first byte.
	DefaultHeaderTimeout = 20 * time.Second
	// DefaultMaxFileBytes is the largest file the console downloads through the control plane.
	DefaultMaxFileBytes int64 = 2 << 30
	credentialLifetime        = 90 * time.Second
	maxErrorBody              = 4 << 10
	maxListBody               = 256 << 10
)

var (
	ErrInvalidConfiguration = errors.New("inbox dispatcher configuration is invalid")
	// ErrMachineOffline: the machine is not online, not allowed to receive files, or not manageable by the actor.
	ErrMachineOffline = errors.New("inbox machine is offline")
	// ErrMachineUnreachable: the machine did not answer, or answered something unusable.
	ErrMachineUnreachable = errors.New("inbox machine is unreachable")
	ErrFileNotFound       = errors.New("inbox file not found")
	// ErrMachineOutdated: the machine runs a Paperboat that cannot list its Inbox yet.
	ErrMachineOutdated = errors.New("inbox machine runs an older Paperboat")
	ErrFileTooLarge    = errors.New("inbox file is too large to download here")
	// ErrCapabilityDisabled: the machine has turned file receiving off.
	ErrCapabilityDisabled = errors.New("inbox capability is disabled on the machine")
)

// Request names one file on one machine for one actor.
type Request struct {
	ActorID        string
	MachineID      string
	FileID         string
	OperationID    string
	RequestID      string
	CorrelationID  string
	IdempotencyKey string
	// TeamID is the selected team workspace, empty for Personal.
	TeamID sql.NullString
}

// MaxListItems is how many files a listing holds; Total counts them all.
const MaxListItems = 200

// Item is one file in a machine's Inbox as the machine lists it.
type Item struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	ReceivedAt time.Time `json:"received_at"`
}

// Listing is a machine's Inbox, newest first, as it is right now.
type Listing struct {
	Total int    `json:"total"`
	Items []Item `json:"items"`
}

// File is a streaming read of one Inbox file. The caller closes Body.
type File struct {
	Body io.ReadCloser
	Size int64
	Name string
}

type Config struct {
	DB            *db.DB
	Signer        *mint.Provider
	Issuer        string
	Client        *http.Client
	HeaderTimeout time.Duration
	MaxFileBytes  int64
	Now           func() time.Time
	NewJTI        func() (string, error)
}

type Dispatcher struct {
	db            *db.DB
	signer        *mint.Provider
	issuer        string
	client        *http.Client
	headerTimeout time.Duration
	maxFileBytes  int64
	now           func() time.Time
	newJTI        func() (string, error)
	resolve       func(context.Context, Request) (route, error)
}

type route struct {
	EnvironmentID string
	OwnerID       string
	BaseURL       string
}

func New(config Config) (*Dispatcher, error) {
	if config.DB == nil || config.Signer == nil || strings.TrimSpace(config.Issuer) == "" {
		return nil, ErrInvalidConfiguration
	}
	d := &Dispatcher{db: config.DB, signer: config.Signer, issuer: strings.TrimSpace(config.Issuer), headerTimeout: config.HeaderTimeout, maxFileBytes: config.MaxFileBytes, now: config.Now, newJTI: config.NewJTI}
	if d.headerTimeout <= 0 {
		d.headerTimeout = DefaultHeaderTimeout
	}
	if d.maxFileBytes <= 0 {
		d.maxFileBytes = DefaultMaxFileBytes
	}
	if d.now == nil {
		d.now = func() time.Time { return time.Now().UTC() }
	}
	if d.newJTI == nil {
		d.newJTI = func() (string, error) {
			value, err := uuid.NewRandom()
			return "token_" + value.String(), err
		}
	}
	client := config.Client
	if client == nil {
		client = &http.Client{}
	}
	// A redirect would forward a one-shot credential to another host. There is no overall timeout:
	// a download may take as long as the caller keeps reading; the caller bounds idle time.
	copied := *client
	copied.Timeout = 0
	copied.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("inbox dispatch redirect rejected") }
	d.client = &copied
	d.resolve = d.resolveRoute
	return d, nil
}

// eligibleMachine returns the machine's owner and runtime environment when the actor may manage the
// machine (its owner, or an owner or admin of its team), it is online, and it receives files.
func (d *Dispatcher) eligibleMachine(ctx context.Context, request Request) (route, error) {
	var found route
	err := d.db.SQL().QueryRowContext(ctx, `SELECT m.user_id,m.environment_id FROM paperboat.user_machines m
 WHERE m.id=$1 AND paperboat.machine_management_allowed($2,m.id) AND ($3::text IS NULL OR m.owner_team_id=$3)
  AND m.state='online' AND m.seat_state='occupied' AND m.online AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND m.disconnected_at IS NULL
  AND m.configured_capabilities @> ARRAY['file_receive']::text[] AND m.observed_capabilities @> ARRAY['file_receive']::text[]`, request.MachineID, request.ActorID, request.TeamID).Scan(&found.OwnerID, &found.EnvironmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return route{}, ErrMachineOffline
	}
	return found, err
}

// resolveRoute adds the machine's runtime ingress to an eligible machine.
func (d *Dispatcher) resolveRoute(ctx context.Context, request Request) (route, error) {
	found, err := d.eligibleMachine(ctx, request)
	if err != nil {
		return route{}, err
	}
	helper, err := d.db.Queries().GetActiveHelperRouteForMachine(ctx, dbsqlc.GetActiveHelperRouteForMachineParams{MachineID: request.MachineID, AccountID: found.OwnerID})
	if errors.Is(err, sql.ErrNoRows) {
		return route{}, errors.Join(ErrMachineUnreachable, err)
	}
	if err != nil {
		return route{}, err
	}
	found.BaseURL, err = runtimeattachment.IngressURL(ctx, d.db, found.EnvironmentID, helper.PublicHost)
	if err != nil {
		return route{}, errors.Join(ErrMachineUnreachable, err)
	}
	return found, nil
}

// List asks the machine for its Inbox right now. Nothing is stored here.
func (d *Dispatcher) List(ctx context.Context, request Request) (Listing, error) {
	request.FileID = ""
	response, err := d.call(ctx, http.MethodGet, "inbox_list", "inbox:list", request)
	if err != nil {
		return Listing{}, err
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxListBody+1))
	if response.StatusCode == http.StatusNotFound {
		// A listing always exists on a machine that serves it; 404 means the route is not there.
		return Listing{}, ErrMachineOutdated
	}
	if response.StatusCode != http.StatusOK {
		return Listing{}, statusError(response.StatusCode)
	}
	var listing Listing
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if readErr != nil || len(body) > maxListBody || decoder.Decode(&listing) != nil || !validListing(listing) {
		return Listing{}, errors.Join(ErrMachineUnreachable, errors.New("inbox listing is invalid"))
	}
	if listing.Items == nil {
		listing.Items = []Item{}
	}
	return listing, nil
}

func validListing(listing Listing) bool {
	if listing.Total < len(listing.Items) || len(listing.Items) > MaxListItems {
		return false
	}
	seen := make(map[string]bool, len(listing.Items))
	for _, item := range listing.Items {
		if len(item.ID) != 16 || seen[item.ID] || item.Name == "" || len(item.Name) > 255 || !utf8.ValidString(item.Name) || strings.ContainsAny(item.Name, "/\\\x00\r\n") || item.Size < 0 || item.ReceivedAt.IsZero() {
			return false
		}
		seen[item.ID] = true
	}
	return true
}

// Open starts a streaming read of one Inbox file.
func (d *Dispatcher) Open(ctx context.Context, request Request) (File, error) {
	response, err := d.call(ctx, http.MethodGet, "inbox_read", "inbox:read", request)
	if err != nil {
		return File{}, err
	}
	size, parseErr := strconv.ParseInt(response.Header.Get("Content-Length"), 10, 64)
	if response.StatusCode != http.StatusOK || parseErr != nil || size < 0 {
		response.Body.Close()
		if response.StatusCode == http.StatusOK {
			return File{}, errors.Join(ErrMachineUnreachable, errors.New("inbox response has no size"))
		}
		return File{}, statusError(response.StatusCode)
	}
	if size > d.maxFileBytes {
		response.Body.Close()
		return File{}, ErrFileTooLarge
	}
	name := fileNameFromDisposition(response.Header.Get("Content-Disposition"))
	return File{Body: &limitedBody{ReadCloser: response.Body, remaining: size}, Size: size, Name: name}, nil
}

// fileNameFromDisposition reads the file name the machine announced in Content-Disposition (the edge
// strips private X-Paperboat-* response headers, so the name cannot travel in one).
func fileNameFromDisposition(value string) string {
	_, params, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	return params["filename"]
}

// Delete removes one Inbox file.
func (d *Dispatcher) Delete(ctx context.Context, request Request) error {
	response, err := d.call(ctx, http.MethodDelete, "inbox_delete", "inbox:delete", request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxErrorBody))
	if response.StatusCode == http.StatusNoContent || response.StatusCode == http.StatusOK {
		return nil
	}
	return statusError(response.StatusCode)
}

func statusError(status int) error {
	switch status {
	case http.StatusNotFound:
		return ErrFileNotFound
	case http.StatusConflict:
		return ErrCapabilityDisabled
	default:
		return errors.Join(ErrMachineUnreachable, fmt.Errorf("machine answered HTTP %d", status))
	}
}

func (d *Dispatcher) call(ctx context.Context, method, class, scope string, request Request) (*http.Response, error) {
	listing := class == "inbox_list"
	if d == nil || strings.TrimSpace(request.ActorID) == "" || strings.TrimSpace(request.MachineID) == "" || !listing && strings.TrimSpace(request.FileID) == "" || strings.TrimSpace(request.OperationID) == "" {
		return nil, ErrInvalidConfiguration
	}
	found, err := d.resolve(ctx, request)
	if err != nil {
		return nil, err
	}
	endpoint, err := inboxEndpoint(found.BaseURL, request.FileID, listing)
	if err != nil {
		return nil, errors.Join(ErrMachineUnreachable, err)
	}
	jti, err := d.newJTI()
	if err != nil || jti == "" {
		return nil, errors.Join(ErrMachineUnreachable, errors.New("credential could not be minted"))
	}
	now := d.now().UTC()
	input := mint.CredentialInput{
		Issuer: d.issuer, Audience: "paperboat-machine", Subject: request.ActorID, JTI: jti, IssuedAt: now, ExpiresAt: now.Add(credentialLifetime),
		CredentialClass: class, Scopes: []string{scope}, EnvironmentID: found.EnvironmentID, AccountID: found.OwnerID, MachineID: request.MachineID,
		UserID: request.ActorID, ActorID: request.ActorID, OperationID: request.OperationID, ResourceKind: mint.InboxResourceKind, ResourceID: request.FileID,
		RequestID: request.RequestID, CorrelationID: request.CorrelationID,
	}
	if class == "inbox_delete" {
		input.IdempotencyKey = request.IdempotencyKey
	}
	if listing {
		input.ResourceKind, input.ResourceID = mint.InboxListResourceKind, request.MachineID
	}
	token, err := admission.Sign(d.signer, input)
	if err != nil {
		return nil, errors.Join(ErrMachineUnreachable, errors.New("credential could not be signed"))
	}
	httpRequest, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, errors.Join(ErrMachineUnreachable, err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+token)
	httpRequest.Header.Set("X-Paperboat-Operation-Id", request.OperationID)
	httpRequest.Header.Set("Cache-Control", "no-store")
	client := *d.client
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if base, ok := d.client.Transport.(*http.Transport); ok && base != nil {
		transport = base.Clone()
	}
	transport.ResponseHeaderTimeout = d.headerTimeout
	client.Transport = transport
	response, err := client.Do(httpRequest)
	if err != nil {
		slog.WarnContext(ctx, "inbox request did not reach the machine", "class", class, "machine_id", request.MachineID, "error", err.Error())
		return nil, errors.Join(ErrMachineUnreachable, err)
	}
	switch response.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound, http.StatusConflict:
	default:
		// The user sees "the machine did not answer"; the status tells an operator why. The start of
		// the body names the refusing party (the daemon answers a JSON error code, the edge text).
		head, _ := io.ReadAll(io.LimitReader(response.Body, 256))
		response.Body = readCloser{Reader: io.MultiReader(bytes.NewReader(head), response.Body), Closer: response.Body}
		slog.WarnContext(ctx, "inbox request refused by the machine", "class", class, "machine_id", request.MachineID, "status", response.StatusCode,
			"refusal", strings.ToValidUTF8(strings.Join(strings.Fields(string(head)), " "), "?"), "server_header", response.Header.Get("Server"))
	}
	return response, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

// inboxEndpoint builds the machine's file URL from its ingress base: the base may be a bare host or
// the logical runtime path of a self-hosted edge, never anything with credentials or a query.
func inboxEndpoint(base, fileID string, collection bool) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(base), "/"))
	if err != nil {
		return "", err
	}
	if (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return "", errors.New("machine route is invalid")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		logical, ok := strings.CutPrefix(parsed.Path, "/v1/runtime/")
		if !ok || logical == "" || strings.ContainsAny(logical, "/:@?#") {
			return "", errors.New("machine route is invalid")
		}
	}
	if collection {
		parsed.Path = strings.TrimRight(parsed.Path, "/") + "/v1/inbox-files"
		parsed.RawPath = ""
		return parsed.String(), nil
	}
	if fileID == "" || strings.ContainsAny(fileID, "/?#% \t\r\n") {
		return "", errors.New("file id is invalid")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/v1/inbox-files/" + fileID
	parsed.RawPath = ""
	return parsed.String(), nil
}

// limitedBody ends a read at the announced size and reports a short body as an error, so a machine
// that stops early cannot make a truncated file look complete.
type limitedBody struct {
	io.ReadCloser
	remaining int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	switch {
	case b.remaining == 0:
		return n, io.EOF
	case errors.Is(err, io.EOF):
		return n, io.ErrUnexpectedEOF
	default:
		return n, err
	}
}
