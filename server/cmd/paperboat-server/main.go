package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/app"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/mintwithdrawal"
	"github.com/pinksaucepasta/paperboat/server/internal/teaminbox"
)

func main() {
	if err := run(os.Args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		printUsage(stdout)
		return nil
	}

	switch args[1] {
	case "version", "--version":
		_, err := fmt.Fprintf(stdout, "paperboat-server %s\n", buildinfo.Version)
		return err
	case "serve":
		return runServe(args[2:], stdout, stderr)
	case "migrate":
		return runMigrate(args[2:], stdout, stderr)
	case "db-role-defaults":
		return runDBRoleDefaults(args[2:], stdout, stderr)
	case "admin":
		return runAdmin(args[2:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[1])
	}
}

const adminUsage = "usage: paperboat-server admin usage-key provision [flags] | admin mint-key withdraw [flags]"

// runAdminWithdrawMintKey takes a mint signing key out of service on every running server (F21-08).
func runAdminWithdrawMintKey(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("admin mint-key withdraw", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to JSON config file")
	actorID := fs.String("actor-user-id", "", "administrator user ID")
	keyID := fs.String("key-id", "", "mint signing key ID to withdraw")
	reason := fs.String("reason", "", "why the key is withdrawn")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("admin mint-key withdraw does not accept positional arguments")
	}
	cfg, err := config.Load(context.Background(), config.LoadOptions{Environment: os.Getenv("PAPERBOAT_ENV"), FilePath: *configPath, LookupEnv: os.LookupEnv, ReadFile: os.ReadFile})
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	store, err := db.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := mintwithdrawal.Withdraw(context.Background(), store.Pool(), *keyID, cfg.CLIAuth.MintActiveKeyID, *actorID, *reason); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "mint signing key %s withdrawn; running servers refuse it now\n", strings.TrimSpace(*keyID))
	return err
}

func runAdmin(args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		return errors.New(adminUsage)
	}
	switch args[0] + " " + args[1] {
	case "usage-key provision":
		return runAdminProvisionUsageKey(args[2:], stdout, stderr)
	case "mint-key withdraw":
		return runAdminWithdrawMintKey(args[2:], stdout, stderr)
	default:
		return errors.New(adminUsage)
	}
}

func runAdminProvisionUsageKey(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("admin usage-key provision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to JSON config file")
	actorID := fs.String("actor-user-id", "", "administrator user ID")
	idempotencyKey := fs.String("idempotency-key", "", "unique operation key")
	keyID := fs.String("key-id", "", "usage signing key ID")
	nodeID := fs.String("edge-node-id", "", "edge node ID")
	publicKeyValue := fs.String("public-key", "", "base64url Ed25519 public key")
	notBeforeValue := fs.String("not-before", "", "RFC3339 validity start")
	expiresAtValue := fs.String("expires-at", "", "RFC3339 validity end")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("admin usage-key provision does not accept positional arguments")
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(*publicKeyValue))
	if err != nil {
		return errors.New("public-key must be unpadded base64url")
	}
	notBefore, err := time.Parse(time.RFC3339, strings.TrimSpace(*notBeforeValue))
	if err != nil {
		return errors.New("not-before must be RFC3339")
	}
	expiresAt, err := time.Parse(time.RFC3339, strings.TrimSpace(*expiresAtValue))
	if err != nil {
		return errors.New("expires-at must be RFC3339")
	}
	cfg, err := config.Load(context.Background(), config.LoadOptions{Environment: os.Getenv("PAPERBOAT_ENV"), FilePath: *configPath, LookupEnv: os.LookupEnv, ReadFile: os.ReadFile})
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	store, err := db.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	service := controlplane.NewEdgeService(store, cfg.Secrets.EdgeControlCredential)
	service.SetAuditWriter(audit.NewWriter(store))
	if err := service.ProvisionUsageKey(context.Background(), strings.TrimSpace(*actorID), strings.TrimSpace(*idempotencyKey), strings.TrimSpace(*keyID), strings.TrimSpace(*nodeID), publicKey, notBefore.UTC(), expiresAt.UTC()); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "usage verification key %s provisioned for %s\n", strings.TrimSpace(*keyID), strings.TrimSpace(*nodeID))
	return err
}

func runMigrate(args []string, stdout, stderr io.Writer) error {
	cfg, err := loadCommandConfig(args, stderr)
	if err != nil {
		return err
	}
	store, err := db.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := db.Migrate(context.Background(), store); err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, "database migrations applied")
	return err
}

// runDBRoleDefaults stores the session settings as defaults of the connecting role. Run it once as the application
// role before a deployment sets database.startup_params to false; it needs no migrations.
func runDBRoleDefaults(args []string, stdout, stderr io.Writer) error {
	cfg, err := loadCommandConfig(args, stderr)
	if err != nil {
		return err
	}
	store, err := db.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.SetRoleDefaults(context.Background(), cfg.Database); err != nil {
		return fmt.Errorf("set database role defaults: %w", err)
	}
	_, err = fmt.Fprintln(stdout, "database role defaults set for the connecting role: search_path, jit, statement_timeout, lock_timeout, idle_in_transaction_session_timeout")
	return err
}

func loadCommandConfig(args []string, stderr io.Writer) (config.Config, error) {
	fs := flag.NewFlagSet("command", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to JSON config file")
	if err := fs.Parse(args); err != nil {
		return config.Config{}, err
	}
	cfg, err := config.Load(context.Background(), config.LoadOptions{
		Environment: os.Getenv("PAPERBOAT_ENV"),
		FilePath:    *configPath,
		LookupEnv:   os.LookupEnv,
		ReadFile:    os.ReadFile,
	})
	if err != nil {
		return config.Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func runServe(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to JSON config file")
	dumpConfig := fs.Bool("dump-config", false, "print redacted config and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(context.Background(), config.LoadOptions{
		Environment: os.Getenv("PAPERBOAT_ENV"),
		FilePath:    *configPath,
		LookupEnv:   os.LookupEnv,
		ReadFile:    os.ReadFile,
	})
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if *dumpConfig {
		_, err := fmt.Fprintln(stdout, cfg.RedactedJSON())
		return err
	}
	if strings.TrimSpace(cfg.Secrets.WorkOSClientID) == "" || strings.TrimSpace(cfg.Secrets.WorkOSAPIKey) == "" {
		return errors.New("PAPERBOAT_WORKOS_CLIENT_ID and PAPERBOAT_WORKOS_API_KEY are required to serve")
	}

	handler := slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(handler)
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var receiptSender teaminbox.ReceiptSender
	if cfg.Notifications.ReceiptEmailEndpoint != "" {
		receiptSender, err = teaminbox.NewHTTPReceiptSender(cfg.Notifications.ReceiptEmailEndpoint, cfg.Secrets.ReceiptEmailToken, cfg.Notifications.ReceiptEmailFrom, &http.Client{Timeout: cfg.HTTP.RequestTimeout})
		if err != nil {
			return fmt.Errorf("configure receipt email delivery: %w", err)
		}
	}
	server, err := app.New(app.Options{
		Config:             cfg,
		Logger:             logger,
		ReceiptEmailSender: receiptSender,
	})
	if err != nil {
		return err
	}
	if err := server.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `paperboat-server controls the Paperboat backend.

Usage:
  paperboat-server serve [-config path] [-dump-config]
  paperboat-server migrate
  paperboat-server db-role-defaults [-config path]
  paperboat-server admin usage-key provision [flags]
`)
}
