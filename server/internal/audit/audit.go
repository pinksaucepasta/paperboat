package audit

import (
	"context"
	"github.com/google/uuid"

	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	serverdiagnostics "github.com/pinksaucepasta/paperboat-server/internal/diagnostics"
)

type ActorType string

const (
	ActorUser   ActorType = "user"
	ActorSystem ActorType = "system"
	ActorAdmin  ActorType = "admin"
	// ActorDirectory is the team's company directory (directory sync, PLAN F19-05/06).
	ActorDirectory ActorType = "directory"
)

// Result values. A denied event names its Reason (a stable code such as team_sso_required).
const (
	ResultSuccess = "success"
	ResultDenied  = "denied"
)

type Event struct {
	ID          string
	ActorUserID string
	// ActorTokenID names the API token that acted for ActorUserID, when one did.
	ActorTokenID   string
	Result         string
	Reason         string
	ActorType      ActorType
	EventType      string
	ResourceType   string
	ResourceID     string
	IdempotencyKey string
	Metadata       map[string]any
	CreatedAt      time.Time
}

type Writer struct {
	db *db.DB
}

func NewWriter(store *db.DB) *Writer {
	return &Writer{db: store}
}

func (w *Writer) Write(ctx context.Context, event Event) error {
	if w == nil || w.db == nil {
		return nil
	}
	return write(ctx, w.db.Queries(), event)
}

func (w *Writer) WriteTx(ctx context.Context, tx *db.Tx, event Event) error {
	if w == nil || tx == nil {
		return nil
	}
	return write(ctx, tx.Queries(), event)
}

func write(ctx context.Context, q *dbsqlc.Queries, event Event) error {
	if event.ID == "" {
		event.ID = newID("event")
	}
	if event.ActorType == "" {
		event.ActorType = ActorSystem
	}
	event.Metadata = maps.Clone(event.Metadata)
	if event.Metadata == nil {
		event.Metadata = make(map[string]any)
	}
	if requestID := serverdiagnostics.NormalizeRequestID(serverdiagnostics.RequestID(ctx)); requestID != "" {
		event.Metadata["request_id"] = requestID
	}
	if event.Result == "" {
		event.Result = ResultSuccess
	}
	client := ClientFrom(ctx)
	if event.ActorTokenID == "" {
		event.ActorTokenID = client.TokenID
	}
	metadata, err := json.Marshal(event.Metadata)
	if err != nil {
		return fmt.Errorf("marshal audit metadata: %w", err)
	}
	err = q.InsertAuditEvent(ctx, dbsqlc.InsertAuditEventParams{ID: event.ID, ActorUserID: event.ActorUserID, ActorType: string(event.ActorType), EventType: event.EventType, ResourceType: event.ResourceType, ResourceID: event.ResourceID, IdempotencyKey: event.IdempotencyKey, Metadata: metadata, ActorTokenID: event.ActorTokenID, Result: event.Result, Reason: truncate(event.Reason, 128), ClientIp: truncate(client.IP, 64), UserAgent: truncate(client.UserAgent, 256)})
	if err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}
	return nil
}

type Query struct {
	Limit        int
	ResourceType string
	ResourceID   string
	ActorUserID  string
}

func (w *Writer) List(ctx context.Context, query Query) ([]Event, error) {
	limit := query.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := w.db.Queries().ListAuditEvents(ctx, dbsqlc.ListAuditEventsParams{ResourceType: query.ResourceType, ResourceID: query.ResourceID, ActorUserID: query.ActorUserID, RowLimit: int32(limit)})
	if err != nil {
		return nil, fmt.Errorf("query audit events: %w", err)
	}
	events := make([]Event, 0, len(rows))
	for _, row := range rows {
		event := Event{ID: row.ID, ActorUserID: row.ActorUserID, ActorType: ActorType(row.ActorType), EventType: row.EventType, ResourceType: row.ResourceType, ResourceID: row.ResourceID, IdempotencyKey: row.IdempotencyKey, CreatedAt: row.CreatedAt}
		if err := json.Unmarshal(row.Metadata, &event.Metadata); err != nil {
			return nil, fmt.Errorf("unmarshal audit metadata: %w", err)
		}
		events = append(events, event)
	}
	return events, nil
}

// Client describes the signed-in request an event came from. HTTP middleware stores it in the
// request context; events written outside a request have none.
type Client struct {
	IP, UserAgent, TokenID string
}

type clientKey struct{}

func WithClient(ctx context.Context, client Client) context.Context {
	return context.WithValue(ctx, clientKey{}, client)
}

func ClientFrom(ctx context.Context) Client {
	client, _ := ctx.Value(clientKey{}).(Client)
	return client
}

// truncate keeps at most n bytes of valid UTF-8 and drops control characters.
func truncate(value string, n int) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	for len(value) > n {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return value
}

func newID(prefix string) string { return prefix + "_" + uuid.NewString() }
