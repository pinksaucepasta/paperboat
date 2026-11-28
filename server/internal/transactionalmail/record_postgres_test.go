package transactionalmail

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Record-only mail never reaches the provider and stays readable; with it off,
// dispatch still sends and scrubs.
func TestMailRecordOnlyPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if e := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); e != nil {
		t.Fatal(e)
	}
	store, e := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if e = db.Migrate(context.Background(), store); e != nil {
		t.Fatal(e)
	}
	prefix := "mail_record_" + fmt.Sprint(time.Now().UnixNano())
	defer func() {
		clean, done := context.WithTimeout(context.Background(), time.Second*10)
		defer done()
		_, _ = store.SQL().ExecContext(clean, `DELETE FROM transactional_mail_outbox WHERE mail_id LIKE $1||'%'`, prefix)
	}()
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"id":"accepted-external"}`)
	}))
	defer provider.Close()
	message := Message{From: "sender@example.test", To: []string{"recipient@example.test"}, Subject: "Product notice", HTML: "<p>" + strings.Repeat("long body ", 500) + "</p>", Text: "Notice"}
	enqueue := func(id string) {
		t.Helper()
		if e := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error { return EnqueueTx(ctx, tx, id, nil, message) }); e != nil {
			t.Fatal(e)
		}
	}
	type row struct {
		state, payload   string
		attempts         int
		accepted, leased bool
		provider         sql.NullString
	}
	read := func(id string) (r row) {
		t.Helper()
		if e := store.SQL().QueryRowContext(ctx, `SELECT state,payload::text,attempts,accepted_at IS NOT NULL,lease_until IS NOT NULL,provider_id FROM transactional_mail_outbox WHERE mail_id=$1`, id).Scan(&r.state, &r.payload, &r.attempts, &r.accepted, &r.leased, &r.provider); e != nil {
			t.Fatal(e)
		}
		return r
	}
	intact := func(payload string) {
		t.Helper()
		var got Message
		if e := json.Unmarshal([]byte(payload), &got); e != nil || !reflect.DeepEqual(got, message) {
			t.Fatalf("recorded payload changed: %s", payload)
		}
	}
	// With and without a provider key, recording never calls the provider.
	for name, key := range map[string]string{"keyed": "private-fixture-key", "keyless": ""} {
		service := Service{DB: store, Endpoint: provider.URL, Key: key, RecordOnly: true}
		id := prefix + "_" + name
		enqueue(id)
		// A second pass finds nothing to do: 'recorded' is terminal.
		for range 2 {
			if e = service.Dispatch(ctx); e != nil {
				t.Fatal(e)
			}
		}
		got := read(id)
		if calls.Load() != 0 || got.state != "recorded" || !got.accepted || got.leased || got.attempts != 0 || got.provider.Valid {
			t.Fatalf("%s: calls=%d row=%+v", name, calls.Load(), got)
		}
		intact(got.payload)
	}
	// A live lease belongs to an instance that may be sending: it is left alone.
	service := Service{DB: store, Endpoint: provider.URL, Key: "private-fixture-key", RecordOnly: true}
	leased := prefix + "_leased"
	enqueue(leased)
	if _, e = store.SQL().ExecContext(ctx, `UPDATE transactional_mail_outbox SET lease_until=now()+interval '1 minute' WHERE mail_id=$1`, leased); e != nil {
		t.Fatal(e)
	}
	if e = service.Dispatch(ctx); e != nil {
		t.Fatal(e)
	}
	if got := read(leased); got.state != "queued" {
		t.Fatalf("leased row = %+v", got)
	}
	// Turning the flag off sends what is queued as before and never resends recorded mail.
	if _, e = store.SQL().ExecContext(ctx, `UPDATE transactional_mail_outbox SET lease_until=NULL WHERE mail_id=$1`, leased); e != nil {
		t.Fatal(e)
	}
	service.RecordOnly = false
	for range 2 {
		if e = service.Dispatch(ctx); e != nil {
			t.Fatal(e)
		}
	}
	if got := read(leased); calls.Load() != 1 || got.state != "sent" || got.payload != "{}" || got.provider.String != "accepted-external" {
		t.Fatalf("flag off: calls=%d row=%+v", calls.Load(), got)
	}
	got := read(prefix + "_keyed")
	if got.state != "recorded" {
		t.Fatalf("recorded row changed: %+v", got)
	}
	intact(got.payload)
}
