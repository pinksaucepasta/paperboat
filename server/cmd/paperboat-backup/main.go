// paperboat-backup is a Linux deployment utility. It never accepts passwords or
// connection URLs as arguments. PGSERVICEFILE and PGPASSFILE identify protected
// libpq configuration files on the deployment host.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/backupreport"
	"github.com/pinksaucepasta/paperboat/server/internal/jobmonitor"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

type backupCommandError struct {
	code  string
	cause error
}

func (e *backupCommandError) Error() string { return "backup command: " + e.code }
func (e *backupCommandError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func backupCommandFailure(code string, causes ...error) error {
	return &backupCommandError{code: code, cause: errors.Join(causes...)}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) (runErr error) {
	if len(args) == 0 {
		return errors.New("usage: paperboat-backup backup|verify|report-retry|cleanup [flags]")
	}
	command := args[0]
	if command != "backup" && command != "verify" && command != "report-retry" && command != "cleanup" {
		return errors.New("unknown backup operation")
	}
	flags := flag.NewFlagSet("paperboat-backup", flag.ContinueOnError)
	// flag errors echo arbitrary inputs; expose only our bounded error code.
	flags.SetOutput(io.Discard)
	cfg := backupreport.ProducerConfig{}
	flags.StringVar(&cfg.DatabaseID, "database", "primary", "non-secret database identifier")
	flags.StringVar(&cfg.SourceService, "source-service", "", "libpq backup source service name")
	flags.StringVar(&cfg.MaintenanceService, "maintenance-service", "", "libpq restore-verification maintenance service name")
	flags.StringVar(&cfg.StorageDir, "storage", "", "dedicated protected backup directory")
	flags.StringVar(&cfg.ReportURL, "report-url", "", "HTTPS operations backup reporting endpoint")
	flags.DurationVar(&cfg.Retention, "retention", 30*24*time.Hour, "successful archive retention")
	flags.DurationVar(&cfg.CommandTimeout, "command-timeout", 30*time.Minute, "deadline per PostgreSQL command")
	flags.DurationVar(&cfg.ReportTimeout, "report-timeout", 15*time.Second, "report request deadline")
	flags.Int64Var(&cfg.MaxArchiveBytes, "max-archive-bytes", 10<<30, "maximum compressed archive size")
	backupID := flags.String("backup-run", "", "exact successful backup run to restore into a new temporary database")
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 || (command == "verify") != (*backupID != "") {
		return errors.New("invalid backup arguments")
	}
	var monitor *jobmonitor.Monitor
	var finish func(bool)
	if command == "backup" || command == "verify" {
		variable := "PAPERBOAT_SENTRY_BACKUP_MONITOR"
		if command == "verify" {
			variable = "PAPERBOAT_SENTRY_RESTORE_MONITOR"
		}
		var err error
		monitor, err = jobmonitor.FromEnv(variable)
		if err != nil {
			return err
		}
		finish = monitor.Start()
	}
	defer monitor.Close()
	defer func() {
		if recovered := recover(); recovered != nil {
			monitor.CaptureFailureAt(commandFailureStage(command), "unknown", supportreport.ErrPanic)
			if finish != nil {
				finish(false)
			}
			panic(supportreport.ErrPanic)
		}
		if runErr != nil {
			monitor.CaptureFailureAt(commandFailureStage(command), backupFailureCode(runErr), runErr)
		}
		if finish != nil {
			finish(runErr == nil)
		}
	}()
	// The token is never accepted in argv or printed. Runtime environments should
	// source it from their secret manager or a protected service EnvironmentFile.
	var err error
	cfg.ReportToken, err = reportToken()
	if err != nil {
		return err
	}
	producer, err := backupreport.NewProducer(cfg)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	switch command {
	case "backup", "verify":
		var report backupreport.Report
		if command == "backup" {
			report, err = producer.Backup(ctx)
		} else {
			report, err = producer.Verify(ctx, *backupID)
		}
		if report.RunID != "" && !report.CompletedAt.IsZero() {
			if writeErr := encoder.Encode(report); writeErr != nil {
				return backupCommandFailure("storage_unavailable", err, writeErr)
			}
		}
		return err
	case "report-retry":
		count, err := producer.RetryReports(ctx)
		if err != nil {
			return err
		}
		if writeErr := encoder.Encode(map[string]int{"reports_delivered": count}); writeErr != nil {
			return backupCommandFailure("storage_unavailable", writeErr)
		}
	case "cleanup":
		count, err := producer.Cleanup(ctx)
		if err != nil {
			return err
		}
		if writeErr := encoder.Encode(map[string]int{"runs_removed": count}); writeErr != nil {
			return backupCommandFailure("storage_unavailable", writeErr)
		}
	}
	return nil
}

func commandFailureStage(command string) string {
	switch command {
	case "cleanup":
		return "backup_cleanup"
	case "report-retry":
		return "backup_reporting"
	default:
		return ""
	}
}

func backupFailureCode(err error) string {
	var failure *backupreport.ProducerError
	if errors.As(err, &failure) && failure != nil {
		return failure.Code
	}
	var commandFailure *backupCommandError
	if errors.As(err, &commandFailure) && commandFailure != nil {
		return commandFailure.code
	}
	return "unknown"
}

func reportToken() (string, error) {
	value := os.Getenv("PAPERBOAT_BACKUP_REPORT_TOKEN")
	path := os.Getenv("PAPERBOAT_BACKUP_REPORT_TOKEN_FILE")
	if path == "" {
		return value, nil
	}
	if value != "" {
		return "", backupCommandFailure("invalid_configuration")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", backupCommandFailure("storage_unavailable", err)
	}
	info, err := file.Stat()
	if err != nil {
		return "", backupCommandFailure("storage_unavailable", err, file.Close())
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 513 {
		return "", backupCommandFailure("unsafe_storage", file.Close())
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, 514))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return "", backupCommandFailure("storage_unavailable", readErr, closeErr)
	}
	if len(raw) > 513 {
		return "", backupCommandFailure("unsafe_storage")
	}
	return strings.TrimSuffix(string(raw), "\n"), nil
}
