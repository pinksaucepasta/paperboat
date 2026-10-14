package backupreport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

type producerFixture struct {
	producer *Producer
	mu       sync.Mutex
	reports  [][]byte
	status   int
	commands []string
	fail     func(context.Context, string, []string) error
}

func newProducerFixture(t *testing.T) *producerFixture {
	t.Helper()
	f := &producerFixture{status: http.StatusNoContent}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/operations/backups/reports" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("t", 32) {
			t.Error("unexpected report request")
			w.WriteHeader(401)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var report Report
		if json.Unmarshal(raw, &report) != nil || !validReport(report, time.Now()) {
			t.Error("producer emitted invalid evidence")
			w.WriteHeader(400)
			return
		}
		f.mu.Lock()
		f.reports = append(f.reports, raw)
		status := f.status
		f.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	p, err := NewProducer(ProducerConfig{DatabaseID: "primary", SourceService: "backup_source", MaintenanceService: "backup_maintenance", StorageDir: filepath.Join(t.TempDir(), "storage"), ReportURL: server.URL + "/v1/operations/backups/reports", ReportToken: strings.Repeat("t", 32), Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	f.producer = p
	p.run = func(ctx context.Context, name string, args []string, output io.Writer) error {
		f.commands = append(f.commands, name+" "+strings.Join(args, " "))
		if f.fail != nil {
			if err := f.fail(ctx, name, args); err != nil {
				return err
			}
		}
		if name == "pg_dump" {
			_, err := io.WriteString(output, "a custom archive fixture")
			return err
		}
		return nil
	}
	return f
}

func assertProducerError(t *testing.T, err error, code string) {
	t.Helper()
	var result *ProducerError
	if !errors.As(err, &result) || result.Code != code {
		t.Fatalf("error = %v; want %s", err, code)
	}
}

func TestProducerBackupAndIsolatedRestore(t *testing.T) {
	f := newProducerFixture(t)
	p := f.producer
	backup, err := p.Backup(context.Background())
	if err != nil || backup.Outcome != "succeeded" || backup.Bytes != int64(len("a custom archive fixture")) || len(backup.SHA256) != 64 {
		t.Fatalf("backup = %+v, error = %v", backup, err)
	}
	for _, name := range []string{producerDumpName(backup.RunID), producerReportName(backup.RunID), producerReceiptName(backup.RunID)} {
		info, err := os.Stat(p.path(name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("artifact %s is not protected", name)
		}
	}
	restored, err := p.Verify(context.Background(), backup.RunID)
	if err != nil || restored.Outcome != "succeeded" || restored.BackupRunID != backup.RunID || restored.SHA256 != backup.SHA256 || restored.Bytes != backup.Bytes || !restored.RetainedUntil.IsZero() {
		t.Fatalf("restore = %+v, error = %v", restored, err)
	}
	name := "pb_backup_verify_" + restored.RunID
	want := []string{
		"pg_dump --no-password --format=custom --dbname=service=backup_source",
		"pg_restore --list " + p.path(producerDumpName(backup.RunID)+".partial"),
		"createdb --no-password --template=template0 --maintenance-db=service=backup_maintenance -- " + name,
		"pg_restore --no-password --exit-on-error --no-owner --no-privileges --dbname=service=backup_maintenance dbname=" + name + " " + p.path(producerDumpName(backup.RunID)),
		"dropdb --no-password --if-exists --force --maintenance-db=service=backup_maintenance -- " + name,
	}
	if strings.Join(f.commands, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected PostgreSQL command boundaries: %v", f.commands)
	}
	if _, err = os.Stat(p.path(producerJournalName(restored.RunID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("successful restore left recovery journal")
	}
}

func TestProducerLockCancellationPreservesMixedCause(t *testing.T) {
	f := newProducerFixture(t)
	lock, err := os.OpenFile(f.producer.path(".paperboat-backup.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}()

	cancelCause := errors.New("SECRET cancellation detail")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.Join(context.Canceled, syscall.EIO, cancelCause))
	_, err = f.producer.Backup(ctx)
	assertProducerError(t, err, "operation_cancelled")
	if !errors.Is(err, context.Canceled) || !errors.Is(err, syscall.EIO) || !errors.Is(err, cancelCause) || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("lock cancellation lost its mixed cause or exposed it: %v", err)
	}
}

func TestProducerReportOutageReplaysExactEvidenceAndProtectsPendingArchive(t *testing.T) {
	f := newProducerFixture(t)
	p := f.producer
	f.status = http.StatusServiceUnavailable
	backup, err := p.Backup(context.Background())
	assertProducerError(t, err, "report_failed")
	var statusError producerHTTPStatusError
	if !errors.As(err, &statusError) || statusError.status != http.StatusServiceUnavailable || !strings.Contains(err.Error(), "report_failed") {
		t.Fatalf("report outage did not retain safe response status: %v", err)
	}
	delivered, deliveryErr := p.delivered(backup.RunID)
	if backup.Outcome != "succeeded" || delivered || deliveryErr != nil {
		t.Fatal("report outage changed backup result or claimed delivery")
	}
	p.now = func() time.Time { return backup.RetainedUntil.Add(time.Minute) }
	if removed, err := p.Cleanup(context.Background()); err != nil || removed != 0 {
		t.Fatalf("pending archive cleanup = %d, %v", removed, err)
	}
	if _, err = os.Stat(p.path(producerDumpName(backup.RunID))); err != nil {
		t.Fatal("pending archive lost")
	}
	f.mu.Lock()
	f.status = http.StatusNoContent
	f.mu.Unlock()
	if delivered, err := p.RetryReports(context.Background()); err != nil || delivered != 1 {
		t.Fatalf("retry = %d, %v", delivered, err)
	}
	f.mu.Lock()
	if len(f.reports) != 2 || !bytes.Equal(f.reports[0], f.reports[1]) {
		t.Error("retry changed immutable evidence")
	}
	f.mu.Unlock()
	if len(f.commands) != 2 {
		t.Fatal("report retry launched another backup")
	}
	if err = os.WriteFile(p.path("user-owned.txt"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if removed, err := p.Cleanup(context.Background()); err != nil || removed != 1 {
		t.Fatalf("completed cleanup = %d, %v", removed, err)
	}
	if _, err = os.Stat(p.path("user-owned.txt")); err != nil {
		t.Fatal("cleanup removed unrelated data")
	}
}

func TestProducerFailuresAreReportedWithoutCommandOutput(t *testing.T) {
	for _, failedCommand := range []string{"pg_dump", "pg_restore"} {
		t.Run(failedCommand, func(t *testing.T) {
			f := newProducerFixture(t)
			cause := errors.New("SECRET provider output")
			f.fail = func(_ context.Context, name string, _ []string) error {
				if name == failedCommand {
					return cause
				}
				return nil
			}
			report, err := f.producer.Backup(context.Background())
			code := "backup_failed"
			if failedCommand == "pg_restore" {
				code = "archive_invalid"
			}
			assertProducerError(t, err, code)
			if !errors.Is(err, cause) || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("producer failure lost its cause or exposed its message: %v", err)
			}
			if report.Outcome != "failed" || report.Bytes != 0 || report.SHA256 != "" || !report.RetainedUntil.IsZero() {
				t.Fatal("failed backup retained success evidence")
			}
			entries, _ := os.ReadDir(f.producer.cfg.StorageDir)
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "pb_backup_") || strings.HasPrefix(entry.Name(), "pb_running_") {
					t.Fatal("failed backup left payload or running journal")
				}
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.reports) != 1 || bytes.Contains(f.reports[0], []byte("SECRET")) {
				t.Fatal("failure evidence missing or disclosed provider content")
			}
		})
	}
}

func TestProducerDeliveryFailureJoinsOriginalBackupCause(t *testing.T) {
	f := newProducerFixture(t)
	f.status = http.StatusServiceUnavailable
	cause := errors.New("SECRET pg_dump detail")
	f.fail = func(_ context.Context, name string, _ []string) error {
		if name == "pg_dump" {
			return cause
		}
		return nil
	}
	report, err := f.producer.Backup(context.Background())
	assertProducerError(t, err, "report_failed")
	var statusError producerHTTPStatusError
	if report.Outcome != "failed" || report.FailureCode != "backup_failed" || !errors.Is(err, cause) || !errors.As(err, &statusError) || statusError.status != http.StatusServiceUnavailable || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("failed backup delivery lost the primary or report cause: report=%+v error=%v", report, err)
	}
}

func TestProducerUnsafeReceiptFailsClosedWithoutResendingOrDeletingArchive(t *testing.T) {
	f := newProducerFixture(t)
	p := f.producer
	backup, err := p.Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	receipt := p.path(producerReceiptName(backup.RunID))
	if err = os.Remove(receipt); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "receipt-target")
	if err = os.WriteFile(target, []byte("untrusted"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(target, receipt); err != nil {
		t.Fatal(err)
	}

	if _, err = p.RetryReports(context.Background()); err == nil {
		t.Fatal("unsafe receipt was treated as an absent delivery marker")
	} else {
		assertProducerError(t, err, "unsafe_storage")
	}
	p.now = func() time.Time { return backup.RetainedUntil.Add(time.Minute) }
	if _, err = p.Cleanup(context.Background()); err == nil {
		t.Fatal("unsafe receipt was treated as an absent marker during retention")
	} else {
		assertProducerError(t, err, "unsafe_storage")
	}
	if _, err = os.Stat(p.path(producerDumpName(backup.RunID))); err != nil {
		t.Fatal("failed receipt inspection allowed deletion of the referenced archive")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reports) != 1 || len(f.commands) != 2 {
		t.Fatalf("unsafe receipt caused a resend or new backup command: reports=%d commands=%v", len(f.reports), f.commands)
	}
}

func TestProducerArchiveBoundAndIntegrityFailure(t *testing.T) {
	f := newProducerFixture(t)
	p := f.producer
	p.cfg.MaxArchiveBytes = 4
	_, err := p.Backup(context.Background())
	assertProducerError(t, err, "backup_failed")
	p.cfg.MaxArchiveBytes = 1024
	backup, err := p.Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p.path(producerDumpName(backup.RunID)), []byte("changed archive contents"), 0600); err != nil {
		t.Fatal(err)
	}
	before := len(f.commands)
	restore, err := p.Verify(context.Background(), backup.RunID)
	assertProducerError(t, err, "restore_failed")
	if restore.Outcome != "failed" || restore.SHA256 != backup.SHA256 || len(f.commands) != before {
		t.Fatal("corrupt archive was restored or reference integrity was lost")
	}
}

func TestProducerRestoreCancellationStillCleansOwnDatabase(t *testing.T) {
	f := newProducerFixture(t)
	p := f.producer
	backup, err := p.Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dropped := false
	f.fail = func(commandContext context.Context, name string, args []string) error {
		if name == "pg_restore" && args[0] == "--no-password" {
			cancel()
			return commandContext.Err()
		}
		if name == "dropdb" {
			dropped = true
			if commandContext.Err() != nil {
				t.Error("database cleanup inherited cancelled operation")
			}
		}
		return nil
	}
	restore, err := p.Verify(ctx, backup.RunID)
	assertProducerError(t, err, "report_failed")
	delivered, deliveryErr := p.delivered(restore.RunID)
	if !dropped || restore.Outcome != "failed" || restore.FailureCode != "restore_failed" || delivered || deliveryErr != nil {
		t.Fatal("cancelled restore cleanup or pending evidence lost")
	}
	if count, err := p.RetryReports(context.Background()); err != nil || count != 1 {
		t.Fatalf("cancelled operation evidence recovery = %d, %v", count, err)
	}
}

func TestProducerCleanupFailureKeepsRecoveryOwnership(t *testing.T) {
	f := newProducerFixture(t)
	p := f.producer
	backup, err := p.Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.fail = func(_ context.Context, name string, _ []string) error {
		if name == "dropdb" {
			return errors.New("database still busy")
		}
		return nil
	}
	restore, err := p.Verify(context.Background(), backup.RunID)
	assertProducerError(t, err, "verification_cleanup_failed")
	delivered, deliveryErr := p.delivered(restore.RunID)
	if restore.FailureCode != "verification_cleanup_failed" || !delivered || deliveryErr != nil {
		t.Fatal("cleanup failure evidence not delivered")
	}
	if _, err = os.Stat(p.path(producerJournalName(restore.RunID))); err != nil {
		t.Fatal("cleanup ownership lost")
	}
	_, rawBefore, err := p.readReport(producerReportName(restore.RunID))
	if err != nil {
		t.Fatal(err)
	}
	f.fail = nil
	if _, err = p.RetryReports(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, rawAfter, _ := p.readReport(producerReportName(restore.RunID))
	if !bytes.Equal(rawBefore, rawAfter) {
		t.Fatal("cleanup recovery changed immutable completed evidence")
	}
	if _, err = os.Stat(p.path(producerJournalName(restore.RunID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("recovered cleanup left running journal")
	}
}

func TestProducerCrashRecoveryAndPendingRestoreRetention(t *testing.T) {
	f := newProducerFixture(t)
	p := f.producer
	backup, err := p.Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := p.start("restore", backup)
	if err != nil {
		t.Fatal(err)
	}
	f.status = http.StatusServiceUnavailable
	_, err = p.RetryReports(context.Background())
	assertProducerError(t, err, "report_failed")
	recovered, _, err := p.readReport(producerReportName(interrupted.RunID))
	if err != nil || recovered.FailureCode != "restore_failed" || recovered.Bytes != backup.Bytes || recovered.SHA256 != backup.SHA256 {
		t.Fatalf("restore crash recovery = %+v, %v", recovered, err)
	}
	p.now = func() time.Time { return backup.RetainedUntil.Add(2 * time.Hour) }
	if removed, err := p.Cleanup(context.Background()); err != nil || removed != 0 {
		t.Fatalf("pending restore reference was not retained: %d, %v", removed, err)
	}
	f.mu.Lock()
	f.status = http.StatusNoContent
	f.mu.Unlock()
	if _, err = p.RetryReports(context.Background()); err != nil {
		t.Fatal(err)
	}
	if removed, err := p.Cleanup(context.Background()); err != nil || removed != 2 {
		t.Fatalf("delivered restore/archive cleanup = %d, %v", removed, err)
	}
}

func TestProducerInterruptedDumpIsFailedAndRemoved(t *testing.T) {
	f := newProducerFixture(t)
	p := f.producer
	running, err := p.start("backup", Report{})
	if err != nil {
		t.Fatal(err)
	}
	partial := p.path(producerDumpName(running.RunID) + ".partial")
	if err = os.WriteFile(partial, []byte("unfinished"), 0600); err != nil {
		t.Fatal(err)
	}
	if count, err := p.RetryReports(context.Background()); err != nil || count != 1 {
		t.Fatalf("interrupted dump recovery = %d, %v", count, err)
	}
	if _, err = os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unfinished payload retained")
	}
	recovered, _, _ := p.readReport(producerReportName(running.RunID))
	if recovered.FailureCode != "backup_failed" || len(f.commands) != 0 {
		t.Fatal("recovery launched new backup or invented success")
	}
}

func TestProducerRejectsUnsafeConfigurationAndRedirect(t *testing.T) {
	f := newProducerFixture(t)
	for _, mutate := range []func(*ProducerConfig){
		func(c *ProducerConfig) { c.SourceService = "source password=SECRET" },
		func(c *ProducerConfig) { c.DatabaseID = "secret:identifier" },
		func(c *ProducerConfig) { c.ReportURL = "http://192.0.2.1/v1/operations/backups/reports" },
		func(c *ProducerConfig) {
			c.ReportURL = "https://user:SECRET@example.test/v1/operations/backups/reports"
		},
		func(c *ProducerConfig) { c.ReportURL += "?secret=SECRET" },
		func(c *ProducerConfig) { c.StorageDir = "relative-path" },
	} {
		cfg := f.producer.cfg
		mutate(&cfg)
		_, err := NewProducer(cfg)
		assertProducerError(t, err, "invalid_configuration")
		if strings.Contains(err.Error(), "SECRET") {
			t.Fatal("configuration error exposed input")
		}
	}
	unsafe := t.TempDir()
	if err := os.Chmod(unsafe, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := f.producer.cfg
	cfg.StorageDir = unsafe
	_, err := NewProducer(cfg)
	assertProducerError(t, err, "unsafe_storage")
	symlink := filepath.Join(t.TempDir(), "storage")
	if err = os.Symlink(f.producer.cfg.StorageDir, symlink); err != nil {
		t.Fatal(err)
	}
	cfg.StorageDir = symlink
	_, err = NewProducer(cfg)
	assertProducerError(t, err, "unsafe_storage")
	targetCalled := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalled = true; w.WriteHeader(204) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	f.producer.cfg.ReportURL = redirect.URL + "/v1/operations/backups/reports"
	_, err = f.producer.Backup(context.Background())
	assertProducerError(t, err, "report_failed")
	if targetCalled {
		t.Fatal("report bearer followed redirect")
	}
}

// This opt-in test runs the actual PostgreSQL tools against the deployment's
// dedicated source fixture. It never modifies the source database. Transfer the
// compiled test executable to the database host; do not move ordinary builds.
func TestProducerRealPostgresBackupRestore(t *testing.T) {
	source := os.Getenv("PAPERBOAT_BACKUP_TEST_SOURCE_SERVICE")
	maintenance := os.Getenv("PAPERBOAT_BACKUP_TEST_MAINTENANCE_SERVICE")
	if source == "" || maintenance == "" {
		t.Skip("requires deployment libpq source and maintenance services")
	}
	for _, tool := range []string{"pg_dump", "pg_restore", "createdb", "dropdb", "psql"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("required PostgreSQL tool unavailable: %s", tool)
		}
	}
	databaseID := "backup_producer_" + fmt.Sprint(time.Now().UnixNano())
	var authority *Service
	if dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"); dsn != "" {
		if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
			t.Fatal("backup producer requires isolated test database")
		}
		store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
		if err != nil {
			t.Fatal("backup reporting test database unavailable")
		}
		defer store.Close()
		if err = db.Migrate(context.Background(), store); err != nil {
			t.Fatal("backup reporting test migration failed")
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := store.Pool().Exec(ctx, `DELETE FROM backup_reports WHERE database_id=$1`, databaseID); err != nil {
				t.Error("backup producer report cleanup failed")
			}
			// Audit records are append-only. The runner owns the isolated database
			// and drops it after verification; individual tests must not bypass retention.
		}()
		authority, err = NewService(store, Config{DatabaseID: databaseID, Token: strings.Repeat("x", 32)})
		if err != nil {
			t.Fatal(err)
		}
	}
	reports := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var report Report
		if json.NewDecoder(r.Body).Decode(&report) != nil || !validReport(report, time.Now()) {
			t.Error("actual producer report is invalid")
			w.WriteHeader(400)
			return
		}
		if authority != nil {
			if err := authority.Ingest(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), report); err != nil {
				t.Error("real producer evidence ingestion failed")
				w.WriteHeader(400)
				return
			}
		}
		reports++
		w.WriteHeader(204)
	}))
	defer server.Close()
	p, err := NewProducer(ProducerConfig{DatabaseID: databaseID, SourceService: source, MaintenanceService: maintenance, StorageDir: filepath.Join(t.TempDir(), "storage"), ReportURL: server.URL + "/v1/operations/backups/reports", ReportToken: strings.Repeat("x", 32), CommandTimeout: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	backup, err := p.Backup(ctx)
	if err != nil || backup.Outcome != "succeeded" {
		t.Fatalf("real dump failed: %v", err)
	}
	restore, err := p.Verify(ctx, backup.RunID)
	if err != nil || restore.Outcome != "succeeded" || restore.SHA256 != backup.SHA256 {
		t.Fatalf("real isolated restore failed: %v", err)
	}
	var exists bytes.Buffer
	if err = p.command(ctx, "psql", []string{"--no-password", "--no-psqlrc", "--tuples-only", "--no-align", "--dbname=service=" + maintenance, "--command=SELECT count(*) FROM pg_database WHERE datname='pb_backup_verify_" + restore.RunID + "'"}, &exists); err != nil || strings.TrimSpace(exists.String()) != "0" {
		t.Fatal("verification database cleanup was not confirmed")
	}
	if reports != 2 {
		t.Fatal("actual backup and restore evidence not received")
	}
	// Exercise a real restore connection failure after the disposable database
	// has been created. The production cleanup must still drop that exact DB.
	realRun := p.run
	p.run = func(commandContext context.Context, name string, args []string, output io.Writer) error {
		if name == "pg_restore" && len(args) > 0 && args[0] == "--no-password" {
			args = append([]string(nil), args...)
			for i, arg := range args {
				if strings.HasPrefix(arg, "--dbname=") {
					args[i] = arg + "_absent"
				}
			}
		}
		return realRun(commandContext, name, args, output)
	}
	failed, err := p.Verify(ctx, backup.RunID)
	assertProducerError(t, err, "restore_failed")
	if failed.Outcome != "failed" || failed.SHA256 != backup.SHA256 {
		t.Fatal("real restore failure lost exact backup reference")
	}
	exists.Reset()
	if err = p.command(ctx, "psql", []string{"--no-password", "--no-psqlrc", "--tuples-only", "--no-align", "--dbname=service=" + maintenance, "--command=SELECT count(*) FROM pg_database WHERE datname='pb_backup_verify_" + failed.RunID + "'"}, &exists); err != nil || strings.TrimSpace(exists.String()) != "0" {
		t.Fatal("failed restore left its verification database")
	}
	if reports != 3 {
		t.Fatal("real restore failure evidence not received")
	}
}
