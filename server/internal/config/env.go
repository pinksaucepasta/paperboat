package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

func overlayEnv(c *Config, lookup func(string) (string, bool), readFile func(string) ([]byte, error)) error {
	if v, ok := lookup("PAPERBOAT_NETWORK_STUN_SERVERS"); ok {
		c.Network.STUNServers = nil
		if v != "" {
			c.Network.STUNServers = strings.Split(v, ",")
		}
	}
	setString := func(name string, target *string) {
		if v, ok := lookup(name); ok {
			*target = v
		}
	}
	setSecret := func(name string, target *string) error {
		if v, ok := lookup(name); ok {
			*target = v
		}
		if path, ok := lookup(name + "_FILE"); ok {
			b, err := readFile(path)
			if err != nil {
				return fmt.Errorf("read %s: %w", name+"_FILE", err)
			}
			*target = strings.TrimSpace(string(b))
		}
		return nil
	}

	if err := setSecret("PAPERBOAT_RESEND_API_KEY", &c.Secrets.ResendAPIKey); err != nil {
		return err
	}
	setString("PAPERBOAT_MAIL_ENDPOINT", &c.Mail.Endpoint)
	setString("PAPERBOAT_MAIL_FROM", &c.Mail.From)
	setString("PAPERBOAT_MAIL_CONSOLE_ORIGIN", &c.Mail.ConsoleOrigin)
	if v, ok := lookup("PAPERBOAT_INVITATION_LIFETIME_SECONDS"); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return errors.New("PAPERBOAT_INVITATION_LIFETIME_SECONDS must be integer")
		}
		c.Mail.InvitationLifetimeSeconds = n
	}

	if v, ok := lookup("PAPERBOAT_SENTRY_ENABLED"); ok && strings.TrimSpace(v) != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("PAPERBOAT_SENTRY_ENABLED must be boolean")
		}
		c.Reporting.Enabled = parsed
	}
	setString("PAPERBOAT_SENTRY_API_BASE_URL", &c.Reporting.APIBaseURL)
	setString("PAPERBOAT_SENTRY_ORGANIZATION", &c.Reporting.Organization)
	if v, ok := lookup("PAPERBOAT_SENTRY_PROJECTS"); ok {
		c.Reporting.Projects = nil
		if strings.TrimSpace(v) != "" {
			for _, project := range strings.Split(v, ",") {
				c.Reporting.Projects = append(c.Reporting.Projects, strings.TrimSpace(project))
			}
		}
	}
	for name, target := range map[string]**bool{"PAPERBOAT_SENTRY_LOGS_ENABLED": &c.Reporting.LogsEnabled, "PAPERBOAT_SENTRY_METRICS_ENABLED": &c.Reporting.MetricsEnabled} {
		if v, ok := lookup(name); ok {
			parsed, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("%s must be boolean", name)
			}
			*target = &parsed
		}
	}
	if v, ok := lookup("PAPERBOAT_SENTRY_TRACES_SAMPLE_RATE"); ok {
		parsed, err := strconv.ParseFloat(v, 64)
		if err != nil || !(parsed >= 0 && parsed <= 1) {
			return errors.New("PAPERBOAT_SENTRY_TRACES_SAMPLE_RATE must be between 0 and 1")
		}
		c.Reporting.TracesSampleRate = &parsed
	}
	setString("PAPERBOAT_BACKUP_DATABASE_ID", &c.Backups.DatabaseID)
	for name, target := range map[string]*int64{"PAPERBOAT_BACKUP_MAX_AGE_SECONDS": &c.Backups.MaxAgeSeconds, "PAPERBOAT_BACKUP_RESTORE_MAX_AGE_SECONDS": &c.Backups.RestoreMaxAgeSeconds} {
		if value, ok := lookup(name); ok {
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return fmt.Errorf("%s must be an integer", name)
			}
			*target = n
		}
	}
	if err := setSecret("PAPERBOAT_SENTRY_API_TOKEN", &c.Secrets.SentryAPIToken); err != nil {
		return err
	}
	if err := setSecret("PAPERBOAT_BACKUP_REPORT_TOKEN", &c.Secrets.BackupReportToken); err != nil {
		return err
	}
	if value, ok := lookup("PAPERBOAT_WORKER_STALL_TIMEOUT"); ok {
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed < time.Minute || parsed > 24*time.Hour {
			return errors.New("PAPERBOAT_WORKER_STALL_TIMEOUT must be a duration between one minute and 24 hours")
		}
		c.Reporting.WorkerStallTimeout = parsed
	}
	setString("PAPERBOAT_SENTRY_HEALTH_MONITOR", &c.Reporting.HealthMonitor)
	setString("PAPERBOAT_SENTRY_REGION", &c.Reporting.Region)
	setString("PAPERBOAT_SENTRY_INSTANCE", &c.Reporting.Instance)
	setString("PAPERBOAT_SENTRY_ISSUE_URL", &c.Reporting.IssueURL)
	// Compose commonly passes an optional variable as an empty string. An empty
	// direct value must not erase an official artifact's embedded destination
	// or release; a nonempty runtime value remains an explicit override, and
	// a secret file remains an explicit deployment-owned override.
	if v, ok := lookup("PAPERBOAT_SENTRY_RELEASE"); ok && strings.TrimSpace(v) != "" {
		c.Reporting.Release = v
	}
	if v, ok := lookup("PAPERBOAT_SENTRY_DSN"); ok && strings.TrimSpace(v) != "" {
		c.Secrets.SentryDSN = v
	}
	if path, ok := lookup("PAPERBOAT_SENTRY_DSN_FILE"); ok {
		b, err := readFile(path)
		if err != nil {
			return fmt.Errorf("read PAPERBOAT_SENTRY_DSN_FILE: %w", err)
		}
		c.Secrets.SentryDSN = strings.TrimSpace(string(b))
	}
	setString("PAPERBOAT_ENV", (*string)(&c.Environment))
	setString("PAPERBOAT_HTTP_ADDRESS", &c.HTTP.Address)
	setString("PAPERBOAT_SYNC_ADDRESS", &c.Sync.Address)
	setString("PAPERBOAT_PUBLIC_BASE_URL", &c.HTTP.PublicBaseURL)
	setString("PAPERBOAT_BROWSER_LOGIN_ORIGIN", &c.HTTP.BrowserLoginOrigin)
	setString("PAPERBOAT_DATABASE_DRIVER", &c.Database.Driver)
	setString("PAPERBOAT_DATABASE_DSN", &c.Database.DSN)
	setString("PAPERBOAT_CONFIG_SYNC_HOME", &c.ConfigSync.HomeOverride)
	setString("PAPERBOAT_CONFIG_SYNC_MODE", &c.ConfigSync.Mode)
	setString("PAPERBOAT_CONFIG_SYNC_POLICY_REVISION", &c.ConfigSync.PolicyRevision)
	setString("PAPERBOAT_CONFIG_SYNC_WARNING_REVISION", &c.ConfigSync.WarningRevision)
	setString("PAPERBOAT_CONFIG_SYNC_MANIFEST_CONTRACT", &c.ConfigSync.ManifestContract)
	setString("PAPERBOAT_CLI_VERIFICATION_URL", &c.CLIAuth.VerificationURL)
	setString("PAPERBOAT_MACHINES_URL", &c.CLIAuth.MachinesURL)
	setString("PAPERBOAT_CLI_CLIENT_ID", &c.CLIAuth.ClientID)
	setString("PAPERBOAT_MINT_ACTIVE_KEY_ID", &c.CLIAuth.MintActiveKeyID)
	setString("PAPERBOAT_GITHUB_OAUTH_AUTHORIZE_URL", &c.GitHub.OAuthAuthorizeURL)
	setString("PAPERBOAT_GITHUB_OAUTH_TOKEN_URL", &c.GitHub.OAuthTokenURL)
	setString("PAPERBOAT_GITHUB_APP_ID", &c.GitHub.AppID)
	setString("PAPERBOAT_GITHUB_CONFIG_REPO_NAME", &c.GitHub.ConfigRepoName)
	setString("PAPERBOAT_GITHUB_CONFIG_REPO_BRANCH", &c.GitHub.ConfigRepoBranch)
	setString("PAPERBOAT_RUNTIME_BASE_DOMAIN", &c.RuntimeBaseDomain)
	setString("PAPERBOAT_LOCAL_ACCESS_BASE_DOMAIN", &c.LocalAccessBaseDomain)
	setString("PAPERBOAT_WORKOS_BASE_URL", &c.Providers.WorkOS.BaseURL)
	setString("PAPERBOAT_OPERATOR_EMAIL", &c.OperatorBootstrapEmail)
	setString("PAPERBOAT_STRIPE_BASE_URL", &c.Providers.Stripe.BaseURL)
	if value, ok := lookup("PAPERBOAT_BILLING_TOPUP_MAX_QUANTITY"); ok {
		n, err := strconv.Atoi(value)
		if err != nil {
			return errors.New("PAPERBOAT_BILLING_TOPUP_MAX_QUANTITY must be an integer")
		}
		c.Billing.TopupMaxQuantity = n
	}
	setString("PAPERBOAT_STRIPE_PERSONAL_PORTAL_CONFIGURATION_ID", &c.Billing.PersonalPortalConfigurationID)
	setString("PAPERBOAT_STRIPE_TEAM_PORTAL_CONFIGURATION_ID", &c.Billing.TeamPortalConfigurationID)
	setString("PAPERBOAT_GITHUB_BASE_URL", &c.Providers.GitHub.BaseURL)
	setString("PAPERBOAT_PREVIEW_SUBDOMAIN_PREFIX", &c.Access.RouteSubdomainPrefix)
	setString("PAPERBOAT_DIAGNOSTICS_OBJECT_ENDPOINT", &c.Diagnostics.ObjectEndpoint)
	setString("PAPERBOAT_DIAGNOSTICS_OBJECT_REGION", &c.Diagnostics.ObjectRegion)
	setString("PAPERBOAT_DIAGNOSTICS_OBJECT_BUCKET", &c.Diagnostics.ObjectBucket)
	setString("PAPERBOAT_RECEIPT_EMAIL_ENDPOINT", &c.Notifications.ReceiptEmailEndpoint)
	setString("PAPERBOAT_RECEIPT_EMAIL_FROM", &c.Notifications.ReceiptEmailFrom)
	setString("PAPERBOAT_RELEASE_DIRECTORY", &c.ReleaseDirectory)
	setString("PAPERBOAT_RELEASE_BASE_URL", &c.ReleaseBaseURL)
	if v, ok := lookup("PAPERBOAT_RELEASE_AUTHORITY_PUBLIC_KEYS"); ok {
		c.ReleaseAuthority.PublicKeys = splitCSV(v)
	}
	if v, ok := lookup("PAPERBOAT_RELEASE_AUTHORITY_THRESHOLD"); ok {
		parsed, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse PAPERBOAT_RELEASE_AUTHORITY_THRESHOLD: %w", err)
		}
		c.ReleaseAuthority.Threshold = parsed
	}
	if value, ok := lookup("PAPERBOAT_MANAGED_SSH_CLIENT_KEY_ACTIVE_WINDOW_HOURS"); ok {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("parse PAPERBOAT_MANAGED_SSH_CLIENT_KEY_ACTIVE_WINDOW_HOURS: %w", err)
		}
		c.ManagedSSH.ClientKeyActiveWindow = time.Duration(parsed) * time.Hour
	}
	if value, ok := lookup("PAPERBOAT_USER_MACHINES_OFFLINE_AFTER_SECONDS"); ok {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("parse PAPERBOAT_USER_MACHINES_OFFLINE_AFTER_SECONDS: %w", err)
		}
		c.UserMachines.OfflineAfter = time.Duration(parsed) * time.Second
	}
	if value, ok := lookup("PAPERBOAT_USER_MACHINES_HELPER_LISTEN_PORT"); ok {
		parsed, err := strconv.ParseInt(value, 10, 32)
		if err != nil {
			return fmt.Errorf("parse PAPERBOAT_USER_MACHINES_HELPER_LISTEN_PORT: %w", err)
		}
		c.UserMachines.RuntimeListenPort = int32(parsed)
	}
	setString("PAPERBOAT_PREVIEW_BASE_DOMAIN", &c.Preview.BaseDomain)
	setString("PAPERBOAT_TUNNEL_BASE_DOMAIN", &c.Tunnel.BaseDomain)
	if value, ok := lookup("PAPERBOAT_TUNNEL_PUBLIC_TCP_PORT_MIN"); ok {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("parse PAPERBOAT_TUNNEL_PUBLIC_TCP_PORT_MIN: %w", err)
		}
		c.Tunnel.PublicTCPPortMin = parsed
	}
	if value, ok := lookup("PAPERBOAT_TUNNEL_PUBLIC_TCP_PORT_MAX"); ok {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("parse PAPERBOAT_TUNNEL_PUBLIC_TCP_PORT_MAX: %w", err)
		}
		c.Tunnel.PublicTCPPortMax = parsed
	}
	setString("PAPERBOAT_CERTIFICATES_DIRECTORY_URL", &c.Certificates.DirectoryURL)
	setString("PAPERBOAT_CERTIFICATES_ACCOUNT_KID", &c.Certificates.AccountKID)
	setString("PAPERBOAT_CERTIFICATES_ACCOUNT_EMAIL", &c.Certificates.AccountEmail)
	setString("PAPERBOAT_CERTIFICATES_ISSUER", &c.Certificates.Issuer)
	setString("PAPERBOAT_CERTIFICATES_ACCOUNT_KEY_REFERENCE", &c.Certificates.AccountKeyReference)
	setString("PAPERBOAT_CERTIFICATES_MASTER_KEY_REFERENCE", &c.Certificates.MasterKeyReference)
	setString("PAPERBOAT_CERTIFICATES_DNS_PROVIDER", &c.Certificates.DNSProvider)
	setString("PAPERBOAT_CERTIFICATES_DNS_ZONE_ID", &c.Certificates.DNSZoneID)
	setString("PAPERBOAT_CERTIFICATES_PUBLIC_DNS_ZONE_ID", &c.Certificates.PublicDNSZoneID)
	setString("PAPERBOAT_CERTIFICATES_DNS_TOKEN_REFERENCE", &c.Certificates.DNSTokenReference)
	setString("PAPERBOAT_CERTIFICATES_CHALLENGE_ZONE", &c.Certificates.ChallengeZone)
	setString("PAPERBOAT_CERTIFICATES_CAA_RESOLVER", &c.Certificates.CAAResolver)
	setString("PAPERBOAT_CERTIFICATES_DISTRIBUTION_CREDENTIAL_REFERENCE", &c.Certificates.DistributionCredentialReference)
	setString("PAPERBOAT_CERTIFICATES_OWNER_ID", &c.Certificates.OwnerID)
	if value, ok := lookup("PAPERBOAT_CERTIFICATES_ENABLED"); ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("PAPERBOAT_CERTIFICATES_ENABLED: %w", err)
		}
		c.Certificates.Enabled = parsed
	}
	for name, target := range map[string]*time.Duration{
		"PAPERBOAT_BROWSER_SESSIONS_LOGIN_LIFETIME":             &c.BrowserSessions.LoginLifetime,
		"PAPERBOAT_BROWSER_SESSIONS_LOGIN_ROTATE_BEFORE":        &c.BrowserSessions.LoginRotateBefore,
		"PAPERBOAT_BROWSER_SESSIONS_RESOURCE_ABSOLUTE_LIFETIME": &c.BrowserSessions.ResourceAbsoluteLifetime,
		"PAPERBOAT_BROWSER_SESSIONS_RESOURCE_IDLE_LIFETIME":     &c.BrowserSessions.ResourceIdleLifetime,
		"PAPERBOAT_CERTIFICATES_RENEW_BEFORE":                   &c.Certificates.RenewBefore,
		"PAPERBOAT_CERTIFICATES_LOCK_TTL":                       &c.Certificates.LockTTL,
		"PAPERBOAT_CERTIFICATES_DISTRIBUTION_TIMEOUT":           &c.Certificates.DistributionTimeout,
		"PAPERBOAT_CERTIFICATES_EXPIRY_ALERT_WINDOW":            &c.Certificates.ExpiryAlertWindow,
		"PAPERBOAT_CERTIFICATES_MAX_LIFETIME":                   &c.Certificates.MaxCertificateLifetime,
		"PAPERBOAT_CERTIFICATES_ACME_TIMEOUT":                   &c.Certificates.ACMETimeout,
		"PAPERBOAT_CERTIFICATES_PROPAGATION_TIMEOUT":            &c.Certificates.PropagationTimeout,
		"PAPERBOAT_CERTIFICATES_CLEANUP_TIMEOUT":                &c.Certificates.CleanupTimeout,
		"PAPERBOAT_CERTIFICATES_POLL_INTERVAL":                  &c.Certificates.PollInterval,
		"PAPERBOAT_CERTIFICATES_RETRY_BASE":                     &c.Certificates.RetryBase,
		"PAPERBOAT_CERTIFICATES_RETRY_MAX":                      &c.Certificates.RetryMax,
	} {
		if value, ok := lookup(name); ok {
			parsed, err := time.ParseDuration(value)
			if err != nil {
				return fmt.Errorf("parse %s: %w", name, err)
			}
			*target = parsed
		}
	}
	if value, ok := lookup("PAPERBOAT_CERTIFICATES_MAX_ATTEMPTS"); ok {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("parse PAPERBOAT_CERTIFICATES_MAX_ATTEMPTS: %w", err)
		}
		c.Certificates.MaxAttempts = parsed
	}
	if v, ok := lookup("PAPERBOAT_FILE_TRANSFER_POLICY_JSON"); ok {
		decoder := json.NewDecoder(strings.NewReader(v))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&c.Access.FileTransfer); err != nil {
			return fmt.Errorf("parse PAPERBOAT_FILE_TRANSFER_POLICY_JSON: %w", err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return fmt.Errorf("parse PAPERBOAT_FILE_TRANSFER_POLICY_JSON: trailing data")
		}
	}
	if v, ok := lookup("PAPERBOAT_TERMINAL_SESSIONS_MAX_ATTEMPTS_BEFORE_ALERT"); ok {
		parsed, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse PAPERBOAT_TERMINAL_SESSIONS_MAX_ATTEMPTS_BEFORE_ALERT: %w", err)
		}
		c.TerminalSessions.MaxAttemptsBeforeAlert = parsed
	}
	if v, ok := lookup("PAPERBOAT_TERMINAL_SESSIONS_MAX_ACTIVE_PER_PROJECT"); ok {
		parsed, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("parse PAPERBOAT_TERMINAL_SESSIONS_MAX_ACTIVE_PER_PROJECT: %w", err)
		}
		c.TerminalSessions.MaxActivePerProject = parsed
	}
	if v, ok := lookup("PAPERBOAT_TERMINAL_SESSIONS_OPERATION_TIMEOUT"); ok {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parse PAPERBOAT_TERMINAL_SESSIONS_OPERATION_TIMEOUT: %w", err)
		}
		c.TerminalSessions.OperationTimeout = parsed
	}
	if v, ok := lookup("PAPERBOAT_TERMINAL_SESSIONS_RETRY_BACKOFF"); ok {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parse PAPERBOAT_TERMINAL_SESSIONS_RETRY_BACKOFF: %w", err)
		}
		c.TerminalSessions.RetryBackoff = parsed
	}
	if v, ok := lookup("PAPERBOAT_TERMINAL_SESSIONS_WORKER_INTERVAL"); ok {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parse PAPERBOAT_TERMINAL_SESSIONS_WORKER_INTERVAL: %w", err)
		}
		c.TerminalSessions.WorkerInterval = parsed
	}
	if v, ok := lookup("PAPERBOAT_ALLOWED_ORIGINS"); ok {
		c.HTTP.AllowedOrigins = splitCSV(v)
	}
	if v, ok := lookup("PAPERBOAT_TRUSTED_PROXY_CIDRS"); ok {
		c.HTTP.TrustedProxyCIDRs = splitCSV(v)
	}
	if v, ok := lookup("PAPERBOAT_CLI_ALLOWED_SCOPES"); ok {
		c.CLIAuth.AllowedScopes = splitCSV(v)
	}
	for name, target := range map[string]*int64{
		"PAPERBOAT_CONFIG_SYNC_MAX_FILE_BYTES":  &c.ConfigSync.MaxFileBytes,
		"PAPERBOAT_CONFIG_SYNC_MAX_BATCH_BYTES": &c.ConfigSync.MaxBatchBytes,
	} {
		if v, ok := lookup(name); ok {
			parsed, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return fmt.Errorf("parse %s: %w", name, err)
			}
			*target = parsed
		}
	}
	for name, target := range map[string]*time.Duration{
		"PAPERBOAT_CONFIG_SYNC_DEBOUNCE":                &c.ConfigSync.Debounce,
		"PAPERBOAT_CONFIG_SYNC_MIN_PUSH_INTERVAL":       &c.ConfigSync.MinPushInterval,
		"PAPERBOAT_CONFIG_SYNC_MAX_DIRTY_DELAY":         &c.ConfigSync.MaxDirtyDelay,
		"PAPERBOAT_CONFIG_SYNC_REMOTE_POLL_INTERVAL":    &c.ConfigSync.RemotePollInterval,
		"PAPERBOAT_CONFIG_SYNC_SHUTDOWN_FLUSH_TIMEOUT":  &c.ConfigSync.ShutdownFlushTimeout,
		"PAPERBOAT_CONFIG_SYNC_SHUTDOWN_GRACE_PERIOD":   &c.ConfigSync.ShutdownGracePeriod,
		"PAPERBOAT_CONFIG_SYNC_SHUTDOWN_REPORT_TIMEOUT": &c.ConfigSync.ShutdownReportTimeout,
		"PAPERBOAT_CONFIG_SYNC_STALE_HEARTBEAT_AFTER":   &c.ConfigSync.StaleHeartbeatAfter,
	} {
		if v, ok := lookup(name); ok {
			parsed, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("parse %s: %w", name, err)
			}
			*target = parsed
		}
	}
	for name, target := range map[string]*int{
		"PAPERBOAT_CONFIG_SYNC_MANIFEST_MAX_BYTES":         &c.ConfigSync.ManifestMaxBytes,
		"PAPERBOAT_CONFIG_SYNC_MANIFEST_MAX_LINES":         &c.ConfigSync.ManifestMaxLines,
		"PAPERBOAT_CONFIG_SYNC_MANIFEST_MAX_PATTERN_BYTES": &c.ConfigSync.ManifestMaxPatternBytes,
		"PAPERBOAT_CONFIG_SYNC_RETRY_LIMIT":                &c.ConfigSync.RetryLimit,
		"PAPERBOAT_CONFIG_SYNC_SUMMARY_LIMIT":              &c.ConfigSync.SummaryLimit,
	} {
		if v, ok := lookup(name); ok {
			parsed, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("parse %s: %w", name, err)
			}
			*target = parsed
		}
	}
	for name, target := range map[string]*time.Duration{
		"PAPERBOAT_CLI_DEVICE_GRANT_LIFETIME":  &c.CLIAuth.DeviceGrantLifetime,
		"PAPERBOAT_CLI_ACCESS_TOKEN_LIFETIME":  &c.CLIAuth.AccessTokenLifetime,
		"PAPERBOAT_CLI_REFRESH_TOKEN_LIFETIME": &c.CLIAuth.RefreshTokenLifetime,
		"PAPERBOAT_CLI_POLL_INTERVAL":          &c.CLIAuth.PollInterval,
		"PAPERBOAT_CLI_SESSION_TOUCH_INTERVAL": &c.CLIAuth.ClientSessionTouchInterval,
		"PAPERBOAT_MINT_JWKS_MAX_AGE":          &c.CLIAuth.MintJWKSMaxAge,
		"PAPERBOAT_MINT_PROOF_LIFETIME":        &c.CLIAuth.MintProofLifetime,
	} {
		if v, ok := lookup(name); ok {
			parsed, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("parse %s: %w", name, err)
			}
			*target = parsed
		}
	}
	for name, target := range map[string]*int{
		"PAPERBOAT_CLI_MAX_CLIENT_LABEL_LENGTH":     &c.CLIAuth.MaxClientLabelLength,
		"PAPERBOAT_CLI_NETWORK_REQUESTS_PER_MINUTE": &c.CLIAuth.NetworkRequestsPerMinute,
		"PAPERBOAT_CLI_GRANT_POLLS_PER_MINUTE":      &c.CLIAuth.GrantPollsPerMinute,
		"PAPERBOAT_CLI_ACCOUNT_ACTIONS_PER_MINUTE":  &c.CLIAuth.AccountActionsPerMinute,
	} {
		if v, ok := lookup(name); ok {
			parsed, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("parse %s: %w", name, err)
			}
			*target = parsed
		}
	}
	if v, ok := lookup("PAPERBOAT_GITHUB_OAUTH_SCOPES"); ok {
		c.GitHub.OAuthScopes = splitCSV(v)
	}
	if v, ok := lookup("PAPERBOAT_ACCESS_CONNECT_READY_TIMEOUT"); ok {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parse PAPERBOAT_ACCESS_CONNECT_READY_TIMEOUT: %w", err)
		}
		c.Access.ConnectReadyTimeout = parsed
	}
	if v, ok := lookup("PAPERBOAT_ACCESS_CONNECT_POLL_INTERVAL"); ok {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parse PAPERBOAT_ACCESS_CONNECT_POLL_INTERVAL: %w", err)
		}
		c.Access.ConnectPollInterval = parsed
	}
	if v, ok := lookup("PAPERBOAT_MAX_BODY_BYTES"); ok {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("PAPERBOAT_MAX_BODY_BYTES: %w", err)
		}
		c.HTTP.MaxBodyBytes = parsed
	}
	if err := setSecret("PAPERBOAT_ENCRYPTION_KEY", &c.Secrets.EncryptionKey); err != nil {
		return err
	}
	if err := setSecret("PAPERBOAT_WORKOS_API_KEY", &c.Secrets.WorkOSAPIKey); err != nil {
		return err
	}
	if err := setSecret("PAPERBOAT_WORKOS_CLIENT_ID", &c.Secrets.WorkOSClientID); err != nil {
		return err
	}
	if err := setSecret("PAPERBOAT_STRIPE_API_KEY", &c.Secrets.StripeAPIKey); err != nil {
		return err
	}
	if err := setSecret("PAPERBOAT_STRIPE_WEBHOOK_SECRET", &c.Secrets.StripeWebhookSecret); err != nil {
		return err
	}
	if err := setSecret("PAPERBOAT_GITHUB_CLIENT_ID", &c.Secrets.GitHubClientID); err != nil {
		return err
	}
	if err := setSecret("PAPERBOAT_GITHUB_CLIENT_SECRET", &c.Secrets.GitHubClientSecret); err != nil {
		return err
	}
	if err := setSecret("PAPERBOAT_EDGE_CONTROL_CREDENTIAL", &c.Secrets.EdgeControlCredential); err != nil {
		return err
	}
	if err := setSecret("PAPERBOAT_DIAGNOSTICS_ACCESS_KEY", &c.Secrets.DiagnosticsAccessKey); err != nil {
		return err
	}
	if err := setSecret("PAPERBOAT_DIAGNOSTICS_SECRET_KEY", &c.Secrets.DiagnosticsSecretKey); err != nil {
		return err
	}
	if err := setSecret("PAPERBOAT_RECEIPT_EMAIL_TOKEN", &c.Secrets.ReceiptEmailToken); err != nil {
		return err
	}
	if value, ok := lookup("PAPERBOAT_DIAGNOSTICS_FORCE_PATH_STYLE"); ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("PAPERBOAT_DIAGNOSTICS_FORCE_PATH_STYLE: %w", err)
		}
		c.Diagnostics.ForcePathStyle = parsed
	}
	if value, ok := lookup("PAPERBOAT_DIAGNOSTICS_RETENTION"); ok {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("PAPERBOAT_DIAGNOSTICS_RETENTION: %w", err)
		}
		c.Diagnostics.Retention = parsed
	}
	if v, ok := lookup("PAPERBOAT_SESSION_KEYS"); ok {
		c.Secrets.SessionKeys = splitCSV(v)
	}
	if v, ok := lookup("PAPERBOAT_MINT_SIGNING_KEYS"); ok {
		c.Secrets.MintSigningKeys = splitCSV(v)
	}
	if path, ok := lookup("PAPERBOAT_MINT_SIGNING_KEYS_FILE"); ok {
		b, err := readFile(path)
		if err != nil {
			return fmt.Errorf("read PAPERBOAT_MINT_SIGNING_KEYS_FILE: %w", err)
		}
		c.Secrets.MintSigningKeys = splitCSV(string(b))
	}
	if path, ok := lookup("PAPERBOAT_SESSION_KEYS_FILE"); ok {
		b, err := readFile(path)
		if err != nil {
			return fmt.Errorf("read PAPERBOAT_SESSION_KEYS_FILE: %w", err)
		}
		c.Secrets.SessionKeys = splitCSV(string(b))
	}
	return nil
}
