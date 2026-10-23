package backupreport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func validFixture(now time.Time) Report {
	return Report{DatabaseID: "primary", RunID: "backup_run_1", Kind: "backup", StartedAt: now.Add(-time.Minute), CompletedAt: now.Add(-time.Second), Outcome: "succeeded", Bytes: 1024, SHA256: strings.Repeat("a", 64), RetainedUntil: now.Add(7 * 24 * time.Hour)}
}
func TestBackupEvidenceValidationAndStates(t *testing.T) {
	now := time.Now().UTC()
	r := validFixture(now)
	if !validReport(r, now) {
		t.Fatal("valid evidence rejected")
	}
	for _, mutate := range []func(*Report){
		func(r *Report) { r.DatabaseID = "postgres://secret" }, func(r *Report) { r.Bytes = -1 }, func(r *Report) { r.CompletedAt = now.Add(time.Hour) }, func(r *Report) { r.CompletedAt = r.StartedAt.Add(-time.Second) }, func(r *Report) { r.SHA256 = "unverified" }, func(r *Report) { r.RetainedUntil = now.Add(-time.Hour) }, func(r *Report) { r.FailureCode = "raw_password_error" }, func(r *Report) { r.Kind = "restore" },
	} {
		bad := r
		mutate(&bad)
		if validReport(bad, now) {
			t.Errorf("invalid evidence accepted %+v", bad)
		}
	}
	if evidenceState(nil, now, time.Hour) != "unknown" {
		t.Fatal("missing evidence healthy")
	}
	record := &Record{Report: r}
	if evidenceState(record, now, time.Hour) != "current" {
		t.Fatal("recent evidence stale")
	}
	if evidenceState(record, now.Add(2*time.Hour), time.Hour) != "stale" {
		t.Fatal("stale evidence current")
	}
	record.Outcome = "failed"
	if evidenceState(record, now, time.Hour) != "failed" {
		t.Fatal("failure hidden")
	}
	s, err := NewService(nil, Config{Token: strings.Repeat("x", 32)})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Authenticate(strings.Repeat("x", 32)) || s.Authenticate(strings.Repeat("y", 32)) || s.Authenticate("") {
		t.Fatal("credential validation")
	}
	disabled, err := NewService(nil, Config{})
	if err != nil || disabled.Authenticate("") {
		t.Fatal("unconfigured producer accepted")
	}
}
func TestBackupReportsPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	actor := fmt.Sprintf("opr_%032x", uint64(time.Now().UnixNano()))
	databaseID := "backup_" + suffix
	if _, err = store.Pool().Exec(ctx, `INSERT INTO operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@example.test','unused test login','active','operations')`, actor); err != nil {
		t.Fatal(err)
	}
	defer func() {
		store.Pool().Exec(ctx, `DELETE FROM backup_reports WHERE database_id=$1`, databaseID)
		store.Pool().Exec(ctx, `DELETE FROM audit_events WHERE actor_user_id=$1 OR resource_id LIKE $2`, actor, suffix+"%")
		store.Pool().Exec(ctx, `DELETE FROM operator_accounts WHERE id=$1`, actor)
	}()
	token := strings.Repeat("x", 32)
	s, err := NewService(store, Config{DatabaseID: databaseID, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return now }
	status, err := s.Status(ctx, actor)
	if err != nil || status.BackupState != "unknown" || status.LatestBackupRestoreVerified {
		t.Fatalf("empty status %+v %v", status, err)
	}
	emptyHealth, healthErr := s.Health(ctx)
	if healthErr != nil || emptyHealth.Backup.State != HealthUnknown || emptyHealth.Restore.State != HealthUnknown || emptyHealth.Backup.AgeSeconds != nil {
		t.Fatalf("empty health %+v", emptyHealth)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if h, err := s.Health(canceled); h.Backup.State != HealthUnavailable || h.Restore.State != HealthUnavailable || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled health %+v", h)
	}
	r := validFixture(now)
	r.DatabaseID = databaseID
	r.RunID = suffix + "_backup"
	if err = s.Ingest(ctx, "not-authorized", r); !errors.Is(err, ErrForbidden) {
		t.Fatal("wrong token accepted", err)
	}
	wrong := r
	wrong.DatabaseID = "other"
	if err = s.Ingest(ctx, token, wrong); !errors.Is(err, ErrInvalid) {
		t.Fatal("cross database accepted", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.Ingest(ctx, token, r) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal("concurrent replay", err)
		}
	}
	changed := r
	changed.Bytes++
	if err = s.Ingest(ctx, token, changed); !errors.Is(err, ErrConflict) {
		t.Fatal("changed replay", err)
	}
	status, err = s.Status(ctx, actor)
	if err != nil || status.BackupState != "current" || status.RestoreState != "unknown" || status.LatestBackupRestoreVerified {
		t.Fatalf("backup isn't restore: %+v %v", status, err)
	}
	h, healthErr := s.Health(ctx)
	if healthErr != nil || h.Backup.State != HealthCurrent || h.Restore.State != HealthUnknown || h.Backup.AgeSeconds == nil || *h.Backup.AgeSeconds != 1 {
		t.Fatalf("backup health %+v", h)
	}
	s.now = func() time.Time { return now.Add(27 * time.Hour) }
	if h, healthErr = s.Health(ctx); healthErr != nil || h.Backup.State != HealthStale {
		t.Fatalf("stale health %+v", h)
	}
	s.now = func() time.Time { return now.Add(8 * 24 * time.Hour) }
	if h, healthErr = s.Health(ctx); healthErr != nil || h.Backup.State != HealthExpired {
		t.Fatalf("expired health %+v", h)
	}
	s.now = func() time.Time { return now }
	restore := Report{DatabaseID: databaseID, RunID: suffix + "_restore", Kind: "restore", BackupRunID: r.RunID, StartedAt: now, CompletedAt: now, Outcome: "succeeded", Bytes: r.Bytes, SHA256: r.SHA256}
	wrong = restore
	wrong.SHA256 = strings.Repeat("b", 64)
	if err = s.Ingest(ctx, token, wrong); !errors.Is(err, ErrConflict) {
		t.Fatal("wrong restore digest accepted", err)
	}
	if err = s.Ingest(ctx, token, restore); err != nil {
		t.Fatal(err)
	}
	status, err = s.Status(ctx, actor)
	if err != nil || !status.LatestBackupRestoreVerified {
		t.Fatalf("verified restore %+v %v", status, err)
	}
	h, healthErr = s.Health(ctx)
	if healthErr != nil || h.Backup.State != HealthCurrent || h.Restore.State != HealthCurrent || h.Restore.AgeSeconds == nil || *h.Restore.AgeSeconds != 0 {
		t.Fatalf("restore health %+v", h)
	}
	// Late evidence cannot supersede newer failure by receipt order.
	failure := r
	failure.RunID = suffix + "_failed"
	failure.Outcome = "failed"
	failure.FailureCode = "backup_failed"
	failure.Bytes = 0
	failure.SHA256 = ""
	failure.RetainedUntil = time.Time{}
	failure.CompletedAt = now
	if err = s.Ingest(ctx, token, failure); err != nil {
		t.Fatal(err)
	}
	if err = s.Ingest(ctx, token, r); err != nil {
		t.Fatal(err)
	}
	status, err = s.Status(ctx, actor)
	if err != nil || status.BackupState != "failed" || status.LastSuccessfulBackup.RunID != r.RunID || status.LatestBackupRestoreVerified {
		t.Fatalf("failure suppressed %+v %v", status, err)
	}
	h, healthErr = s.Health(ctx)
	if healthErr != nil || h.Backup.State != HealthFailed || h.Restore.State != HealthCurrent {
		t.Fatalf("failure hidden by health %+v", h)
	}
	page, err := s.List(ctx, actor, "", 1)
	if err != nil || len(page.Runs) != 1 || page.NextAfter == "" {
		t.Fatalf("pagination %+v %v", page, err)
	}
	next, err := s.List(ctx, actor, page.NextAfter, 100)
	if err != nil || len(next.Runs) != 2 {
		t.Fatalf("next page %+v %v", next, err)
	}
	s.now = func() time.Time { return now.Add(9 * 24 * time.Hour) }
	if h, healthErr = s.Health(ctx); healthErr != nil || h.Restore.State != HealthStale || h.Backup.State != HealthFailed {
		t.Fatalf("stale restore health %+v", h)
	}
	s.now = func() time.Time { return now.Add(time.Second) }
	recovered := r
	recovered.RunID = suffix + "_recovery"
	recovered.CompletedAt = now.Add(time.Second)
	if err = s.Ingest(ctx, token, recovered); err != nil {
		t.Fatal(err)
	}
	if h, healthErr = s.Health(ctx); healthErr != nil || h.Backup.State != HealthCurrent || h.Backup.AgeSeconds == nil || *h.Backup.AgeSeconds != 0 {
		t.Fatalf("health did not recover %+v", h)
	}
	// Audit failure rolls back evidence; sensitive read discloses nothing.
	function := "reject_backup_" + suffix
	if _, err = store.Pool().Exec(ctx, `CREATE FUNCTION `+function+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_type IN ('backup.report_received','operator.backups_read') THEN RAISE EXCEPTION 'test audit rejection'; END IF; RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Pool().Exec(ctx, `CREATE TRIGGER `+function+` BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION `+function+`() `); err != nil {
		t.Fatal(err)
	}
	defer func() {
		store.Pool().Exec(context.Background(), `DROP TRIGGER IF EXISTS `+function+` ON audit_events`)
		store.Pool().Exec(context.Background(), `DROP FUNCTION IF EXISTS `+function+`() `)
	}()
	bad := r
	bad.RunID = suffix + "_rollback"
	if err = s.Ingest(ctx, token, bad); err == nil {
		t.Fatal("audit failure accepted")
	}
	if out, err := s.Status(ctx, actor); err == nil || out.DatabaseID != "" {
		t.Fatal("audit failure disclosed")
	}
	if _, err = store.Pool().Exec(ctx, `DROP TRIGGER `+function+` ON audit_events`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Pool().Exec(ctx, `DROP FUNCTION `+function+`() `); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = store.Pool().QueryRow(ctx, `SELECT count(*) FROM backup_reports WHERE database_id=$1 AND run_id=$2`, databaseID, bad.RunID).Scan(&count); err != nil || count != 0 {
		t.Fatal("audit rollback", count, err)
	}
	if _, err = store.Pool().Exec(ctx, `UPDATE operator_accounts SET role='support' WHERE id=$1`, actor); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Status(ctx, actor); !errors.Is(err, ErrForbidden) {
		t.Fatal("revoked role disclosed", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	unavailable, err := db.Open(config.Database{Driver: "postgres", DSN: fmt.Sprintf("postgres://healthcheck@127.0.0.1:%d/unavailable?sslmode=disable&connect_timeout=1", port)})
	if err != nil {
		t.Fatal("unavailable health database setup failed")
	}
	defer unavailable.Close()
	s.store = unavailable
	failedHealth, healthErr := s.Health(ctx)
	if !errors.Is(healthErr, syscall.ECONNREFUSED) || failedHealth.Backup.State != HealthUnavailable || failedHealth.Restore.State != HealthUnavailable {
		t.Fatalf("health read hid database outage cause or state: snapshot=%+v error=%v", failedHealth, healthErr)
	}
	s.store = store
	recoveredHealth, healthErr := s.Health(ctx)
	if healthErr != nil || recoveredHealth.Backup.State != HealthCurrent || recoveredHealth.Restore.State != HealthCurrent {
		t.Fatalf("health sampling did not recover on the connected database: snapshot=%+v error=%v", recoveredHealth, healthErr)
	}
}
