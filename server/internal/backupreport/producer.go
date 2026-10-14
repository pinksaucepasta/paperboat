package backupreport

// The producer runs on the Linux database deployment host. PostgreSQL connection
// secrets remain in libpq service/password files; only completed, content-free
// evidence leaves the deployment. Storage must be a dedicated protected directory.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const producerMaxRecords = 2048

var producerID = regexp.MustCompile(`^[0-9a-f]{32}$`)
var producerService = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var producerDatabase = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
var producerTemporary = regexp.MustCompile(`^\.pb-write-[0-9]+$`)

// ProducerError keeps a finite public code while retaining the original
// provider or system cause for a bounded local diagnostic. Error deliberately
// does not format that cause.
type ProducerError struct {
	Code  string
	Cause error
}

func (e *ProducerError) Error() string { return "backup operation: " + e.Code }
func (e *ProducerError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func producerError(code string, causes ...error) error {
	return &ProducerError{Code: code, Cause: errors.Join(causes...)}
}

func producerFailureCause(err error) error {
	var failure *ProducerError
	if errors.As(err, &failure) && failure != nil && failure.Cause != nil {
		return failure.Cause
	}
	return err
}

func producerFailureCode(err error, fallback string) string {
	var failure *ProducerError
	if errors.As(err, &failure) && failure != nil && failure.Code != "" {
		return failure.Code
	}
	return fallback
}

type producerHTTPStatusError struct{ status int }

func (producerHTTPStatusError) Error() string {
	return "backup reporting returned an unsuccessful HTTP status"
}
func (e producerHTTPStatusError) DiagnosticStatus() int { return e.status }

type ProducerConfig struct {
	DatabaseID, SourceService, MaintenanceService string
	StorageDir, ReportURL, ReportToken            string
	Retention, CommandTimeout, ReportTimeout      time.Duration
	MaxArchiveBytes                               int64
}

type Producer struct {
	cfg    ProducerConfig
	client *http.Client
	now    func() time.Time
	run    func(context.Context, string, []string, io.Writer) error
}

func NewProducer(cfg ProducerConfig) (*Producer, error) {
	if !producerDatabase.MatchString(cfg.DatabaseID) || !filepath.IsAbs(cfg.StorageDir) || (cfg.SourceService != "" && !producerService.MatchString(cfg.SourceService)) || (cfg.MaintenanceService != "" && !producerService.MatchString(cfg.MaintenanceService)) {
		return nil, producerError("invalid_configuration")
	}
	u, err := url.Parse(cfg.ReportURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/v1/operations/backups/reports" || (u.Scheme != "https" && !(u.Scheme == "http" && producerLoopback(u.Hostname()))) || len(cfg.ReportToken) < 32 || len(cfg.ReportToken) > 512 || strings.ContainsAny(cfg.ReportToken, " \t\r\n") {
		return nil, producerError("invalid_configuration")
	}
	if cfg.Retention == 0 {
		cfg.Retention = 30 * 24 * time.Hour
	}
	if cfg.CommandTimeout == 0 {
		cfg.CommandTimeout = 30 * time.Minute
	}
	if cfg.ReportTimeout == 0 {
		cfg.ReportTimeout = 15 * time.Second
	}
	if cfg.MaxArchiveBytes == 0 {
		cfg.MaxArchiveBytes = 10 << 30
	}
	if cfg.Retention < time.Hour || cfg.Retention > 365*24*time.Hour || cfg.CommandTimeout < time.Second || cfg.CommandTimeout > 24*time.Hour || cfg.ReportTimeout < time.Second || cfg.ReportTimeout > time.Minute || cfg.MaxArchiveBytes < 1 || cfg.MaxArchiveBytes > 1<<50 {
		return nil, producerError("invalid_configuration")
	}
	cfg.StorageDir = filepath.Clean(cfg.StorageDir)
	if err := os.MkdirAll(cfg.StorageDir, 0700); err != nil {
		return nil, producerError("storage_unavailable", err)
	}
	info, err := os.Lstat(cfg.StorageDir)
	if err != nil {
		return nil, producerError("storage_unavailable", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, producerError("unsafe_storage")
	}
	//paperboat:allow-source-policy default-http owner=backupreport reason=owned_clone_bounded_timeout_proxy_disabled
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	p := &Producer{cfg: cfg, now: func() time.Time { return time.Now().UTC() }, client: &http.Client{Transport: transport, Timeout: cfg.ReportTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	p.run = func(ctx context.Context, name string, args []string, output io.Writer) error {
		command := exec.CommandContext(ctx, name, args...)
		command.Stdout, command.Stderr = output, io.Discard
		command.Stdin = nil
		command.WaitDelay = 5 * time.Second
		return command.Run()
	}
	return p, nil
}

func releaseProducerLock(file *os.File) error {
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}

func producerLoopback(host string) bool {
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

func (p *Producer) path(name string) string { return filepath.Join(p.cfg.StorageDir, name) }
func producerReportName(id string) string   { return "pb_report_" + id + ".json" }
func producerDumpName(id string) string     { return "pb_backup_" + id + ".dump" }
func producerJournalName(id string) string  { return "pb_running_" + id + ".json" }
func producerReceiptName(id string) string  { return "pb_report_" + id + ".sent" }

// withStorageLock also serializes retention against a restore using an archive.
// Kernel locks disappear on process exit; crash recovery never guesses PID liveness.
func (p *Producer) withStorageLock(ctx context.Context, work func() error) (resultErr error) {
	file, err := os.OpenFile(p.path(".paperboat-backup.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return producerError("storage_unavailable", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		closeErr := file.Close()
		if err != nil {
			return producerError("storage_unavailable", errors.Join(err, closeErr))
		}
		return producerError("unsafe_storage", closeErr)
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return producerError("storage_unavailable", errors.Join(err, file.Close()))
		}
		select {
		case <-ctx.Done():
			return producerError("operation_cancelled", errors.Join(ctx.Err(), context.Cause(ctx)), file.Close())
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer func() {
		releaseErr := releaseProducerLock(file)
		if releaseErr == nil {
			return
		}
		if resultErr == nil {
			resultErr = producerError("storage_unavailable", releaseErr)
			return
		}
		// Keep the operation's safe public code and its complete cause tree while
		// making lock-release failure observable to the caller.
		resultErr = producerError(producerFailureCode(resultErr, "storage_unavailable"), resultErr, releaseErr)
	}()
	if ctx.Err() != nil {
		return producerError("operation_cancelled", errors.Join(ctx.Err(), context.Cause(ctx)))
	}
	if err = p.recoverInterrupted(); err != nil {
		return err
	}
	return work()
}

func (p *Producer) syncDir() error {
	dir, err := os.Open(p.cfg.StorageDir)
	if err != nil {
		return producerError("storage_unavailable", err)
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil || closeErr != nil {
		return producerError("storage_unavailable", syncErr, closeErr)
	}
	return nil
}

func (p *Producer) writeFile(name string, raw []byte) error {
	file, err := os.CreateTemp(p.cfg.StorageDir, ".pb-write-*")
	if err != nil {
		return producerError("storage_unavailable", err)
	}
	temporary := file.Name()
	_, writeErr := file.Write(raw)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		cleanupErr := removeProducerFile(temporary)
		return producerError("storage_unavailable", writeErr, closeErr, cleanupErr)
	}
	if err = os.Rename(temporary, p.path(name)); err != nil {
		cleanupErr := removeProducerFile(temporary)
		return producerError("storage_unavailable", err, cleanupErr)
	}
	return p.syncDir()
}

func (p *Producer) readReport(name string) (Report, []byte, error) {
	var report Report
	file, err := os.OpenFile(p.path(name), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return report, nil, producerError("storage_unavailable", err)
	}
	info, err := file.Stat()
	if err != nil {
		return report, nil, producerError("storage_unavailable", errors.Join(err, file.Close()))
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 16<<10 {
		return report, nil, producerError("unsafe_storage", file.Close())
	}
	raw, err := io.ReadAll(io.LimitReader(file, (16<<10)+1))
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return report, nil, producerError("storage_unavailable", err, closeErr)
	}
	if err = json.Unmarshal(raw, &report); err != nil || !producerID.MatchString(report.RunID) || report.DatabaseID != p.cfg.DatabaseID || (report.Kind != "backup" && report.Kind != "restore") || (report.Kind == "restore" && !producerID.MatchString(report.BackupRunID)) {
		return report, nil, producerError("invalid_manifest", err)
	}
	return report, raw, nil
}

func (p *Producer) entries() ([]os.DirEntry, error) {
	dir, err := os.Open(p.cfg.StorageDir)
	if err != nil {
		return nil, producerError("storage_unavailable", err)
	}
	entries, err := dir.ReadDir(producerMaxRecords*5 + 1)
	closeErr := dir.Close()
	if err != nil && !errors.Is(err, io.EOF) || closeErr != nil {
		return nil, producerError("storage_unavailable", err, closeErr)
	}
	if len(entries) > producerMaxRecords*5 {
		return nil, producerError("storage_limit")
	}
	return entries, nil
}

func (p *Producer) start(kind string, backup Report) (Report, error) {
	var report Report
	entries, err := p.entries()
	if err != nil {
		return report, err
	}
	count := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "pb_report_") && strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}
	if count >= producerMaxRecords {
		return report, producerError("storage_limit")
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return report, producerError("entropy_unavailable", err)
	}
	report = Report{DatabaseID: p.cfg.DatabaseID, RunID: hex.EncodeToString(random[:]), Kind: kind, BackupRunID: backup.RunID, StartedAt: p.now(), Bytes: backup.Bytes, SHA256: backup.SHA256}
	raw, err := json.Marshal(report)
	if err != nil {
		return Report{}, producerError("invalid_manifest", err)
	}
	return report, p.writeFile(producerJournalName(report.RunID), raw)
}

func (p *Producer) command(ctx context.Context, name string, args []string, output io.Writer) error {
	bounded, cancel := context.WithTimeout(ctx, p.cfg.CommandTimeout)
	defer cancel()
	if err := p.run(bounded, name, args, output); err != nil {
		return errors.Join(err, bounded.Err())
	}
	return nil
}

type producerBoundedWriter struct {
	writer io.Writer
	left   int64
}

func (w *producerBoundedWriter) Write(raw []byte) (int, error) {
	if int64(len(raw)) > w.left {
		return 0, producerError("archive_limit")
	}
	n, err := w.writer.Write(raw)
	w.left -= int64(n)
	return n, err
}

func (p *Producer) Backup(ctx context.Context) (Report, error) {
	var report Report
	if p.cfg.SourceService == "" {
		return report, producerError("invalid_configuration")
	}
	err := p.withStorageLock(ctx, func() error {
		var err error
		report, err = p.start("backup", Report{})
		if err != nil {
			return err
		}
		partial := p.path(producerDumpName(report.RunID) + ".partial")
		file, err := os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return p.finish(ctx, &report, "backup_failed", err)
		}
		hash := sha256.New()
		writer := &producerBoundedWriter{writer: io.MultiWriter(file, hash), left: p.cfg.MaxArchiveBytes}
		err = p.command(ctx, "pg_dump", []string{"--no-password", "--format=custom", "--dbname=service=" + p.cfg.SourceService}, writer)
		if err == nil {
			err = file.Sync()
		}
		err = errors.Join(err, file.Close())
		failure := ""
		var failureCause error
		if err != nil || writer.left == p.cfg.MaxArchiveBytes {
			failure = "backup_failed"
			failureCause = err
		} else if err = p.command(ctx, "pg_restore", []string{"--list", partial}, io.Discard); err != nil {
			failure = "archive_invalid"
			failureCause = err
		} else if err = os.Rename(partial, p.path(producerDumpName(report.RunID))); err != nil {
			failure = "backup_failed"
			failureCause = err
		} else if err = p.syncDir(); err != nil {
			failure = "backup_failed"
			failureCause = err
		} else {
			report.Bytes = p.cfg.MaxArchiveBytes - writer.left
			report.SHA256 = hex.EncodeToString(hash.Sum(nil))
			report.RetainedUntil = p.now().Add(p.cfg.Retention)
		}
		if failure != "" {
			partialErr := removeProducerFile(partial)
			dumpErr := removeProducerFile(p.path(producerDumpName(report.RunID)))
			if partialErr != nil || dumpErr != nil {
				return producerError("storage_unavailable", failureCause, producerFailureCause(partialErr), producerFailureCause(dumpErr))
			}
		}
		return p.finish(ctx, &report, failure, failureCause)
	})
	return report, err
}

func (p *Producer) Verify(ctx context.Context, backupRunID string) (Report, error) {
	var report Report
	if p.cfg.MaintenanceService == "" || !producerID.MatchString(backupRunID) {
		return report, producerError("invalid_configuration")
	}
	err := p.withStorageLock(ctx, func() error {
		backup, _, err := p.readReport(producerReportName(backupRunID))
		if err != nil {
			return err
		}
		if backup.RunID != backupRunID || backup.Kind != "backup" || backup.Outcome != "succeeded" || !backup.RetainedUntil.After(p.now()) {
			return producerError("backup_unavailable")
		}
		report, err = p.start("restore", backup)
		if err != nil {
			return err
		}
		var failureCause error
		if err = p.checkArchive(ctx, backup); err != nil {
			return p.finish(ctx, &report, "restore_failed", err)
		}
		name := "pb_backup_verify_" + report.RunID
		failure := ""
		if failureCause = p.command(ctx, "createdb", []string{"--no-password", "--template=template0", "--maintenance-db=service=" + p.cfg.MaintenanceService, "--", name}, io.Discard); failureCause != nil {
			failure = "restore_failed"
		} else if failureCause = p.command(ctx, "pg_restore", []string{"--no-password", "--exit-on-error", "--no-owner", "--no-privileges", "--dbname=service=" + p.cfg.MaintenanceService + " dbname=" + name, p.path(producerDumpName(backupRunID))}, io.Discard); failureCause != nil {
			failure = "restore_failed"
		}
		// Always attempt cleanup: createdb can succeed remotely while its client is
		// cancelled, and the exact random database is the only permitted target.
		cleanupErr := p.dropVerification(report.RunID)
		if cleanupErr != nil {
			// Keep the running journal so a future invocation retries cleanup.
			report.CompletedAt, report.Outcome, report.FailureCode = p.now(), "failed", "verification_cleanup_failed"
			raw, _ := json.Marshal(report)
			if err = p.writeFile(producerReportName(report.RunID), raw); err != nil {
				return producerError(producerFailureCode(err, "storage_unavailable"), failureCause, producerFailureCause(cleanupErr), producerFailureCause(err))
			}
			sendErr := p.send(ctx, report)
			return producerError("verification_cleanup_failed", failureCause, producerFailureCause(cleanupErr), producerFailureCause(sendErr))
		}
		return p.finish(ctx, &report, failure, failureCause)
	})
	return report, err
}

func (p *Producer) checkArchive(ctx context.Context, report Report) error {
	file, err := os.OpenFile(p.path(producerDumpName(report.RunID)), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return producerError("backup_unavailable", err)
	}
	info, err := file.Stat()
	if err != nil {
		return producerError("backup_unavailable", errors.Join(err, file.Close()))
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() != report.Bytes || report.Bytes <= 0 || report.Bytes > p.cfg.MaxArchiveBytes {
		return producerError("archive_invalid", file.Close())
	}
	hash := sha256.New()
	_, readErr := io.Copy(hash, producerContextReader{ctx, file})
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return producerError("archive_invalid", readErr, closeErr)
	}
	if hex.EncodeToString(hash.Sum(nil)) != report.SHA256 {
		return producerError("archive_invalid")
	}
	return nil
}

type producerContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r producerContextReader) Read(raw []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(raw)
}

func (p *Producer) dropVerification(id string) error {
	if p.cfg.MaintenanceService == "" || !producerID.MatchString(id) {
		return producerError("verification_cleanup_failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := p.run(ctx, "dropdb", []string{"--no-password", "--if-exists", "--force", "--maintenance-db=service=" + p.cfg.MaintenanceService, "--", "pb_backup_verify_" + id}, io.Discard); err != nil {
		return producerError("verification_cleanup_failed", errors.Join(err, ctx.Err()))
	}
	return nil
}

func (p *Producer) finish(ctx context.Context, report *Report, failure string, failureCause ...error) error {
	primaryCause := errors.Join(failureCause...)
	report.CompletedAt = p.now()
	report.Outcome, report.FailureCode = "succeeded", failure
	if failure != "" {
		report.Outcome = "failed"
	}
	if err := p.persistCompleted(*report); err != nil {
		return producerError(producerFailureCode(err, "storage_unavailable"), primaryCause, producerFailureCause(err))
	}
	if err := p.send(ctx, *report); err != nil {
		return producerError("report_failed", primaryCause, producerFailureCause(err))
	}
	if failure != "" {
		return producerError(failure, primaryCause)
	}
	return nil
}

func (p *Producer) persistCompleted(report Report) error {
	raw, err := json.Marshal(report)
	if err != nil {
		return producerError("invalid_manifest", err)
	}
	if err = p.writeFile(producerReportName(report.RunID), raw); err != nil {
		return err
	}
	if err = removeProducerFile(p.path(producerJournalName(report.RunID))); err != nil {
		return err
	}
	return p.syncDir()
}

func (p *Producer) send(ctx context.Context, report Report) error {
	if report.Kind == "restore" {
		delivered, err := p.delivered(report.BackupRunID)
		if err != nil {
			return err
		}
		if !delivered {
			backup, _, err := p.readReport(producerReportName(report.BackupRunID))
			if err != nil {
				return err
			}
			if backup.RunID != report.BackupRunID || backup.Kind != "backup" || backup.Outcome != "succeeded" {
				return producerError("invalid_manifest")
			}
			if err = p.send(ctx, backup); err != nil {
				return err
			}
		}
	}
	stored, raw, err := p.readReport(producerReportName(report.RunID))
	if err != nil {
		return err
	}
	if stored.RunID != report.RunID || stored.CompletedAt.IsZero() {
		return producerError("invalid_manifest")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.ReportURL, bytes.NewReader(raw))
	if err != nil {
		return producerError("report_failed", err)
	}
	request.Header.Set("Authorization", "Bearer "+p.cfg.ReportToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return producerError("report_failed", err)
	}
	_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return producerError("report_failed", readErr, closeErr, producerHTTPStatusError{status: response.StatusCode})
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusNoContent {
		return producerError("report_failed", producerHTTPStatusError{status: response.StatusCode})
	}
	if err := p.writeFile(producerReceiptName(report.RunID), []byte("delivered\n")); err != nil {
		return producerError("report_failed", producerFailureCause(err))
	}
	return nil
}

func (p *Producer) RetryReports(ctx context.Context) (int, error) {
	count := 0
	err := p.withStorageLock(ctx, func() error {
		entries, err := p.entries()
		if err != nil {
			return err
		}
		// Restores reference backup runs. Deliver backup evidence first after a
		// control-plane outage, independent of random identifier ordering.
		for _, kind := range []string{"backup", "restore"} {
			for _, entry := range entries {
				id, ok := producerFilenameID(entry.Name(), "pb_report_", ".json")
				if !ok {
					continue
				}
				delivered, err := p.delivered(id)
				if err != nil {
					return err
				}
				if delivered {
					continue
				}
				report, _, err := p.readReport(entry.Name())
				if err != nil {
					return err
				}
				if report.RunID != id {
					return producerError("invalid_manifest")
				}
				if report.Kind != kind {
					continue
				}
				if err = p.send(ctx, report); err != nil {
					return err
				}
				count++
			}
		}
		return nil
	})
	return count, err
}

func (p *Producer) delivered(id string) (bool, error) {
	info, err := os.Lstat(p.path(producerReceiptName(id)))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, producerError("storage_unavailable", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return false, producerError("unsafe_storage")
	}
	return true, nil
}

func producerFilenameID(name, prefix, suffix string) (string, bool) {
	id := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
	return id, strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix) && producerID.MatchString(id)
}

func removeProducerFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return producerError("storage_unavailable", err)
	}
	return nil
}

func (p *Producer) recoverInterrupted() error {
	entries, err := p.entries()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if producerTemporary.MatchString(entry.Name()) {
			if err = removeProducerFile(p.path(entry.Name())); err != nil {
				return err
			}
			continue
		}
		if id, receipt := producerFilenameID(entry.Name(), "pb_report_", ".sent"); receipt {
			// Retention unlinks the manifest before its receipt. A crash in that
			// gap leaves only this content-free producer-owned marker.
			_, reportErr := os.Lstat(p.path(producerReportName(id)))
			_, journalErr := os.Lstat(p.path(producerJournalName(id)))
			if errors.Is(reportErr, os.ErrNotExist) && errors.Is(journalErr, os.ErrNotExist) {
				if err = removeProducerFile(p.path(entry.Name())); err != nil {
					return err
				}
			} else if reportErr != nil && !errors.Is(reportErr, os.ErrNotExist) || journalErr != nil && !errors.Is(journalErr, os.ErrNotExist) {
				return producerError("storage_unavailable", reportErr, journalErr)
			}
			continue
		}
		id, ok := producerFilenameID(entry.Name(), "pb_running_", ".json")
		if !ok {
			continue
		}
		report, _, err := p.readReport(entry.Name())
		if err != nil {
			return err
		}
		if report.RunID != id {
			return producerError("invalid_manifest")
		}
		if report.Kind == "restore" {
			if err = p.dropVerification(id); err != nil {
				return err
			}
		}
		if _, err = os.Lstat(p.path(producerReportName(id))); err == nil {
			// A completed report was fsynced before the crash. Never replace its
			// immutable evidence with an invented recovery result.
			if err = removeProducerFile(p.path(entry.Name())); err != nil {
				return err
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return producerError("storage_unavailable", err)
		}
		if report.Kind == "backup" {
			for _, name := range []string{producerDumpName(id), producerDumpName(id) + ".partial"} {
				if err = removeProducerFile(p.path(name)); err != nil {
					return err
				}
			}
		}
		report.CompletedAt, report.Outcome = p.now(), "failed"
		report.FailureCode = "backup_failed"
		if report.Kind == "restore" {
			report.FailureCode = "restore_failed"
		}
		if err = p.persistCompleted(report); err != nil {
			return err
		}
	}
	return nil
}

// Cleanup removes only this producer's exact generated names after retention,
// and preserves every undelivered report and its referenced backup. Operators
// should run report-retry before cleanup; neither action launches a new dump.
func (p *Producer) Cleanup(ctx context.Context) (int, error) {
	count := 0
	err := p.withStorageLock(ctx, func() error {
		entries, err := p.entries()
		if err != nil {
			return err
		}
		reports := make([]Report, 0)
		protected := make(map[string]bool)
		for _, entry := range entries {
			id, ok := producerFilenameID(entry.Name(), "pb_report_", ".json")
			if !ok {
				continue
			}
			report, _, err := p.readReport(entry.Name())
			if err != nil {
				return err
			}
			if report.RunID != id || report.CompletedAt.IsZero() {
				return producerError("invalid_manifest")
			}
			reports = append(reports, report)
			delivered, err := p.delivered(id)
			if err != nil {
				return err
			}
			if !delivered {
				protected[id] = true
				protected[report.BackupRunID] = true
			}
		}
		for _, report := range reports {
			if ctx.Err() != nil {
				return producerError("operation_cancelled", ctx.Err())
			}
			expires := report.RetainedUntil
			if expires.IsZero() {
				expires = report.CompletedAt.Add(p.cfg.Retention)
			}
			if protected[report.RunID] || expires.After(p.now()) {
				continue
			}
			if report.Kind == "backup" {
				if err = removeProducerFile(p.path(producerDumpName(report.RunID))); err != nil {
					return err
				}
				// Persist payload deletion before its only manifest can disappear.
				if err = p.syncDir(); err != nil {
					return err
				}
			}
			if err = removeProducerFile(p.path(producerReportName(report.RunID))); err != nil {
				return err
			}
			if err = removeProducerFile(p.path(producerReceiptName(report.RunID))); err != nil {
				return err
			}
			count++
		}
		return p.syncDir()
	})
	return count, err
}
