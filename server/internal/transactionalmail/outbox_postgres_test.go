package transactionalmail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// The durable boundary owns rollback, lost provider acknowledgement, lease
// recovery and the 24-hour provider idempotency limit; no production test seam.
func TestMailLeaseRetryHorizonPostgres(t *testing.T) {
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
	if e = db.Migrate(ctx, store); e != nil {
		t.Fatal(e)
	}
	id := "mail_crash_" + fmt.Sprint(time.Now().UnixNano())
	defer func() {
		clean, done := context.WithTimeout(context.Background(), time.Second*10)
		defer done()
		_, _ = store.SQL().ExecContext(clean, `DELETE FROM transactional_mail_outbox WHERE mail_id=$1 OR mail_id=$2 OR mail_id=$3`, id, id+"_rollback", id+"_expired")
	}()
	message := Message{From: "sender@example.test", To: []string{"recipient@example.test"}, Subject: "Product notice", HTML: "<p>Notice</p>"}
	stop := errors.New("rollback product mutation")
	e = store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if e := EnqueueTx(ctx, tx, id+"_rollback", nil, message); e != nil {
			return e
		}
		return stop
	})
	if !errors.Is(e, stop) {
		t.Fatal(e)
	}
	var count int
	if e = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM transactional_mail_outbox WHERE mail_id=$1`, id+"_rollback").Scan(&count); e != nil || count != 0 {
		t.Fatal("rolled back product queued mail")
	}
	if e = store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error { return EnqueueTx(ctx, tx, id, nil, message) }); e != nil {
		t.Fatal(e)
	}
	sendCtx, lostAck := context.WithCancel(ctx)
	var mu sync.Mutex
	keys := []string{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		// The provider must be able to lock the claimed row: its claim transaction
		// was committed before the external HTTP request started.
		tx, e := store.SQL().BeginTx(ctx, nil)
		if e != nil {
			t.Error("provider could not inspect committed claim")
		} else {
			var locked string
			e = tx.QueryRowContext(ctx, `SELECT mail_id FROM transactional_mail_outbox WHERE mail_id=$1 FOR UPDATE NOWAIT`, id).Scan(&locked)
			_ = tx.Rollback()
			if e != nil || locked != id {
				t.Error("database transaction held over provider HTTP")
			}
		}
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		first := len(keys) == 1
		mu.Unlock()
		// The external provider has accepted the request, but the caller disconnects
		// before local confirmation. The production claim must remain recoverable.
		if first {
			lostAck()
		}
		_, _ = io.WriteString(w, `{"id":"accepted-external"}`)
	}))
	defer provider.Close()
	service := Service{DB: store, Endpoint: provider.URL, Key: "private-fixture-key"}
	_ = service.Dispatch(sendCtx)
	var state string
	var accepted *time.Time
	if e = store.SQL().QueryRowContext(ctx, `SELECT state,accepted_at FROM transactional_mail_outbox WHERE mail_id=$1`, id).Scan(&state, &accepted); e != nil || state != "queued" || accepted != nil {
		t.Fatal("lost acknowledgement fabricated acceptance")
	}
	// Advance only the expired lease, then the same durable request reconciles.
	if _, e = store.SQL().ExecContext(ctx, `UPDATE transactional_mail_outbox SET lease_until=now()-interval '1 second',next_attempt_at=now() WHERE mail_id=$1`, id); e != nil {
		t.Fatal(e)
	}
	if e = service.Dispatch(ctx); e != nil {
		t.Fatal(e)
	}
	mu.Lock()
	same := len(keys) == 2 && keys[0] == keys[1] && keys[0] == id
	mu.Unlock()
	if !same {
		t.Fatal("lease recovery changed provider idempotency key")
	}
	if e = store.SQL().QueryRowContext(ctx, `SELECT state,accepted_at FROM transactional_mail_outbox WHERE mail_id=$1`, id).Scan(&state, &accepted); e != nil || state != "sent" || accepted == nil {
		t.Fatal("reconciled acceptance missing")
	}
	// A real pending request beyond provider retention must never be sent again.
	if e = store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error { return EnqueueTx(ctx, tx, id+"_expired", nil, message) }); e != nil {
		t.Fatal(e)
	}
	if _, e = store.SQL().ExecContext(ctx, `UPDATE transactional_mail_outbox SET attempts=1,first_attempt_at=now()-interval '24 hours' WHERE mail_id=$1`, id+"_expired"); e != nil {
		t.Fatal(e)
	}
	if e = service.Dispatch(ctx); e != nil {
		t.Fatal(e)
	}
	mu.Lock()
	n := len(keys)
	mu.Unlock()
	if n != 2 {
		t.Fatal("request retried beyond provider idempotency retention")
	}
	if e = store.SQL().QueryRowContext(ctx, `SELECT state FROM transactional_mail_outbox WHERE mail_id=$1`, id+"_expired").Scan(&state); e != nil || state != "failed" {
		t.Fatal("exhausted retention not terminal")
	}
}
