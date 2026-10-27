package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/server/internal/admission"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/shared/edgelimits"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Environment string

const (
	EnvironmentDevelopment Environment = "development"
	EnvironmentTest        Environment = "test"
	EnvironmentProduction  Environment = "production"
)

type BackupReporting struct {
	DatabaseID           string `json:"database_id"`
	MaxAgeSeconds        int64  `json:"max_age_seconds"`
	RestoreMaxAgeSeconds int64  `json:"restore_max_age_seconds"`
}

type Reporting struct {
	WorkerStallTimeout time.Duration `json:"worker_stall_timeout"`
	HealthMonitor      string        `json:"health_monitor"`
	Region             string        `json:"region"`
	Instance           string        `json:"instance"`
	APIBaseURL         string        `json:"api_base_url"`
	Organization       string        `json:"organization"`
	Projects           []string      `json:"projects"`
	Enabled            bool          `json:"enabled"`
	LogsEnabled        *bool         `json:"logs_enabled,omitempty"`
	MetricsEnabled     *bool         `json:"metrics_enabled,omitempty"`
	TracesSampleRate   *float64      `json:"traces_sample_rate,omitempty"`
	Release            string        `json:"release"`
	IssueURL           string        `json:"issue_url"`
}

type Config struct {
	Mail                   TransactionalMail `json:"mail"`
	OperatorBootstrapEmail string            `json:"operator_bootstrap_email"`
	Backups                BackupReporting   `json:"backups"`
	Reporting              Reporting         `json:"reporting"`
	Environment            Environment       `json:"environment"`
	// SelfhostAllowPrivateIngress lets self-hosted node claim and ingress verification accept private
	// (RFC 1918 / ULA) addresses: a staging stack on a LAN. Refused in production, where it would open the
	// claim dialer to internal addresses.
	SelfhostAllowPrivateIngress bool `json:"selfhost_allow_private_ingress"`
	// EdgeIngressLimits are the per-publication and per-account ingress limits every edge enforces (T11), sent
	// to edges in the heartbeat answer. One set for all plans, from this one place.
	EdgeIngressLimits     edgelimits.IngressLimits `json:"edge_ingress_limits"`
	LocalAccessBaseDomain string                   `json:"local_access_base_domain"`
	RuntimeBaseDomain     string                   `json:"runtime_base_domain"`
	BrowserSessions       BrowserSessions          `json:"browser_sessions"`
	HTTP                  HTTPConfig               `json:"http"`
	Database              Database                 `json:"database"`
	Billing               Billing                  `json:"billing"`
	Business              Business                 `json:"business"`
	APITokens             APITokens                `json:"api_tokens"`
	RateLimits            RateLimits               `json:"rate_limits"`
	UserMachines          UserMachines             `json:"user_machines"`
	ManagedSSH            ManagedSSH               `json:"managed_ssh"`
	TerminalSessions      TerminalSessions         `json:"terminal_sessions"`
	Preview               Preview                  `json:"preview"`
	Tunnel                Tunnel                   `json:"tunnel"`
	TestFixtures          TestFixtures             `json:"test_fixtures"`
	Certificates          Certificates             `json:"certificates"`
	ConfigSync            ConfigSync               `json:"config_sync"`
	CLIAuth               CLIAuth                  `json:"cli_auth"`
	GitHub                GitHub                   `json:"github"`
	Access                Access                   `json:"access"`
	Diagnostics           Diagnostics              `json:"diagnostics"`
	Retention             Retention                `json:"retention"`
	Notifications         Notifications            `json:"notifications"`
	Providers             Providers                `json:"providers"`
	Secrets               Secrets                  `json:"secrets"`
	ReleaseDirectory      string                   `json:"release_directory"`
	ReleaseBaseURL        string                   `json:"release_base_url"`
	ReleaseAuthority      ReleaseAuthority         `json:"release_authority"`
	WorkspacePolicy       WorkspacePolicy          `json:"workspace_policy"`
}

type HTTPConfig struct {
	BrowserLoginOrigin string        `json:"browser_login_origin"`
	Address            string        `json:"address"`
	PublicBaseURL      string        `json:"public_base_url"`
	AllowedOrigins     []string      `json:"allowed_origins"`
	ReadHeaderTimeout  time.Duration `json:"read_header_timeout"`
	RequestTimeout     time.Duration `json:"request_timeout"`
	ShutdownTimeout    time.Duration `json:"shutdown_timeout"`
	IdleTimeout        time.Duration `json:"idle_timeout"`
	MaxHeaderBytes     int           `json:"max_header_bytes"`
	MaxBodyBytes       int64         `json:"max_body_bytes"`
	TrustedProxyCIDRs  []string      `json:"trusted_proxy_cidrs"`
}

// NormalizeIssuer returns the canonical server identity used in CLI
// connection descriptors and helper credentials. It intentionally mirrors
// the CLI's issuer normalization so equivalent URLs cannot fail validation.
func NormalizeIssuer(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return strings.TrimRight(strings.TrimSpace(raw), "/")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	hostname := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port == "" {
		if strings.Contains(hostname, ":") {
			u.Host = "[" + hostname + "]"
		} else {
			u.Host = hostname
		}
	} else {
		u.Host = net.JoinHostPort(hostname, port)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

type Database struct {
	Driver string `json:"driver"`
	DSN    string `json:"dsn"`
	// DirectDSN reaches PostgreSQL without a connection pooler (empty: DSN). Session state (advisory locks held
	// across statements, LISTEN) and migrations use that direct pool, because a transaction-pooled connection can
	// hand the next statement of a session to another backend.
	DirectDSN string `json:"direct_dsn"`
	// StartupParams sends search_path, jit and the timeouts as connection startup parameters. Unset means true.
	// Set it false behind a pooler that rejects startup parameters, after `paperboat-server db-role-defaults` has
	// stored the same values as role defaults.
	StartupParams *bool `json:"startup_params"`
	// MaxConns bounds the request pool (0: 20). Statement, lock and idle-in-transaction timeouts bound every
	// connection of it (0: 30 s, 10 s, 60 s), so a stuck query or a lock queue cannot hold a connection forever;
	// migrations run on their own pool without them.
	MaxConns                 int32         `json:"max_conns"`
	StatementTimeout         time.Duration `json:"statement_timeout"`
	LockTimeout              time.Duration `json:"lock_timeout"`
	IdleInTransactionTimeout time.Duration `json:"idle_in_transaction_timeout"`
	// AcquireTimeout bounds the wait for a request-pool connection (0: 3 s): a full pool answers 503 with
	// Retry-After instead of holding the request until its own deadline.
	AcquireTimeout time.Duration `json:"acquire_timeout"`
}

// SendsStartupParams reports whether connections start with the session settings as startup parameters.
func (d Database) SendsStartupParams() bool { return d.StartupParams == nil || *d.StartupParams }

// APITokens bounds workspace API tokens (F18).
type APITokens struct {
	MaxLifetimeDays     int `json:"max_lifetime_days"`
	DefaultLifetimeDays int `json:"default_lifetime_days"`
	MaxPerMember        int `json:"max_per_member"`
	// RetrySeconds is how long a create with an Idempotency-Key can be repeated and gets the same
	// token and once-shown secret back (held encrypted for this window only).
	RetrySeconds int `json:"retry_seconds"`
}

// RateLimits bound sign-in and credential attempts per client network, in memory, before any
// database work (PLAN F21-14).
type RateLimits struct {
	// SignInPerMinute/SignInBurst: browser, CLI device, API-token and operator sign-in requests.
	SignInPerMinute int `json:"sign_in_per_minute"`
	SignInBurst     int `json:"sign_in_burst"`
	// TokenFailuresPerMinute/TokenFailuresBurst: failed API-token authentications on any route.
	TokenFailuresPerMinute int `json:"token_failures_per_minute"`
	TokenFailuresBurst     int `json:"token_failures_burst"`
	// MachinePeerNetworkConfigPerMinute/Burst: a machine's signed-network configuration refreshes (about 6 a minute
	// when idle, more right after changes); a looping client gets 429.
	MachinePeerNetworkConfigPerMinute int `json:"machine_peer_network_config_per_minute"`
	MachinePeerNetworkConfigBurst     int `json:"machine_peer_network_config_burst"`
	// MaxClients bounds the memory of each limiter.
	MaxClients int `json:"max_clients"`
}

type Billing struct {
	TopupMaxQuantity              int    `json:"topup_max_quantity"`
	PersonalPortalConfigurationID string `json:"personal_portal_configuration_id,omitempty"`
	TeamPortalConfigurationID     string `json:"team_portal_configuration_id,omitempty"`
	PersonalAllowanceBytes        *int64 `json:"personal_allowance_bytes,omitempty"`
	TeamAllowanceBytes            *int64 `json:"team_allowance_bytes,omitempty"`
	// TeamTrialDays is the Team plan's free trial for an account's first team; 0 disables it.
	TeamTrialDays int `json:"team_trial_days"`
	// Limits are the per-plan count limits (user decision 2026-10-23), the one place they live.
	Limits PlanLimits `json:"limits"`
}

// PlanLimit is one count limit per plan. Free and Pro apply to a personal workspace. Teams are
// advertised as unlimited; TeamCeiling is a hidden per-team safety ceiling, never shown on pricing.
type PlanLimit struct {
	Free        int `json:"free"`
	Pro         int `json:"pro"`
	TeamCeiling int `json:"team_ceiling"`
}

// PlanLimits counts only live things (revoked, deleted, closed or expired ones never count);
// existing ones keep working when a workspace is over, only a new one is refused.
type PlanLimits struct {
	Machines PlanLimit `json:"machines"`
	// Publications are previews and tunnels together, concurrently active.
	Publications PlanLimit `json:"publications"`
	// BrowserTerminals are console terminals open through the edge; native terminals are unlimited.
	BrowserTerminals PlanLimit `json:"browser_terminals"`
	// ViewersPerSharedTerminal caps one shared terminal's viewers (abuse protection; the shared
	// terminal itself counts as one session, its viewers do not).
	ViewersPerSharedTerminal int `json:"viewers_per_shared_terminal"`
	// PublicTCP bounds concurrent public raw-TCP tunnels (user decision 2026-10-27).
	PublicTCP PublicTCPLimit `json:"public_tcp"`
}

// PublicTCPLimit is the concurrent public raw-TCP tunnel limit: Free and Pro per personal workspace, a team
// gets TeamPerSeat for every paid seat, pooled. A workspace on its own (self-hosted) edge is not limited.
type PublicTCPLimit struct {
	Free        int `json:"free"`
	Pro         int `json:"pro"`
	TeamPerSeat int `json:"team_per_seat"`
}

func (l PlanLimit) valid() bool {
	return l.Free > 0 && l.Pro >= l.Free && l.TeamCeiling >= l.Pro
}

// ManagedSSH bounds how long an idle CLI session keeps counting toward SSH client-key capacity
// and authorized keys; a session used within the window keeps its key, an older one stops counting.
type ManagedSSH struct {
	ClientKeyActiveWindow time.Duration `json:"client_key_active_window"`
}

type UserMachines struct {
	PairingLifetime time.Duration `json:"pairing_lifetime"`
	OfflineAfter    time.Duration `json:"offline_after"`
	// ExpiredAccessSessionRetention is how long an expired access session may still be named as
	// admitted before it is retired.
	ExpiredAccessSessionRetention time.Duration `json:"expired_access_session_retention"`
	AllowedPlatforms              []string      `json:"allowed_platforms"`
	RuntimeListenPort             int32         `json:"runtime_listen_port"`
}

type TerminalSessions struct {
	OperationTimeout       time.Duration `json:"operation_timeout"`
	RetryBackoff           time.Duration `json:"retry_backoff"`
	WorkerInterval         time.Duration `json:"worker_interval"`
	MaxAttemptsBeforeAlert int           `json:"max_attempts_before_alert"`
	// OperationMaxAge ends a close or delete_history the host has not applied for this long (the machine stayed
	// unreachable); the host's orphan reconcile handles the session if it returns.
	OperationMaxAge time.Duration `json:"operation_max_age"`
}

type Preview struct {
	BaseDomain string `json:"base_domain"`
}

// TestFixtures confines the final re-check runner's DNS records: names must sit under Domain, in the Cloudflare zone
// ZoneID (the server's own DNS token writes them). The runner's token is Secrets.TestFixturesToken; without it nothing
// is mounted.
type TestFixtures struct {
	Domain string `json:"domain"`
	ZoneID string `json:"zone_id"`
}

type Tunnel struct {
	BaseDomain string `json:"base_domain"`
}

// Certificates contains references and public endpoints only. It never
// carries an ACME account key, DNS token, envelope key, or edge credential.
// Values are resolved by the server process from its configured secret
// provider when the managed lifecycle is enabled.
type Certificates struct {
	Enabled      bool   `json:"enabled"`
	DirectoryURL string `json:"directory_url"`
	AccountKID   string `json:"account_kid"`
	AccountEmail string `json:"account_email"`
	Issuer       string `json:"issuer"`
	// Challenge selects the ACME challenge type: dns-01 (default, Cloudflare)
	// or http-01 (staging only; the edge answers /.well-known/acme-challenge).
	Challenge           string `json:"challenge"`
	AccountKeyReference string `json:"account_key_reference"`
	MasterKeyReference  string `json:"master_key_reference"`
	DNSProvider         string `json:"dns_provider"`
	DNSZoneID           string `json:"dns_zone_id"`
	PublicDNSZoneID     string `json:"public_dns_zone_id"`
	DNSTokenReference   string `json:"dns_token_reference"`
	ChallengeZone       string `json:"challenge_zone"`
	// DNSWriteBase, when set, confines every Cloudflare write to TXT records at or under this name
	// (a staging server sharing a token with a production zone).
	DNSWriteBase                    string        `json:"dns_write_base"`
	CAAResolver                     string        `json:"caa_resolver"`
	DistributionCredentialReference string        `json:"distribution_credential_reference"`
	OwnerID                         string        `json:"owner_id"`
	RenewBefore                     time.Duration `json:"renew_before"`
	LockTTL                         time.Duration `json:"lock_ttl"`
	DistributionTimeout             time.Duration `json:"distribution_timeout"`
	ExpiryAlertWindow               time.Duration `json:"expiry_alert_window"`
	MaxCertificateLifetime          time.Duration `json:"max_certificate_lifetime"`
	ACMETimeout                     time.Duration `json:"acme_timeout"`
	PropagationTimeout              time.Duration `json:"propagation_timeout"`
	CleanupTimeout                  time.Duration `json:"cleanup_timeout"`
	PollInterval                    time.Duration `json:"poll_interval"`
	MaxAttempts                     int           `json:"max_attempts"`
	RetryBase                       time.Duration `json:"retry_base"`
	RetryMax                        time.Duration `json:"retry_max"`
}

type ConfigSync struct {
	Mode                    string        `json:"mode"`
	HomeOverride            string        `json:"home_override"`
	ManifestContract        string        `json:"manifest_contract"`
	ManifestMaxBytes        int           `json:"manifest_max_bytes"`
	ManifestMaxLines        int           `json:"manifest_max_lines"`
	ManifestMaxPatternBytes int           `json:"manifest_max_pattern_bytes"`
	MaxFileBytes            int64         `json:"max_file_bytes"`
	MaxBatchBytes           int64         `json:"max_batch_bytes"`
	Debounce                time.Duration `json:"debounce"`
	MinPushInterval         time.Duration `json:"min_push_interval"`
	MaxDirtyDelay           time.Duration `json:"max_dirty_delay"`
	RemotePollInterval      time.Duration `json:"remote_poll_interval"`
	RetryLimit              int           `json:"retry_limit"`
	ShutdownFlushTimeout    time.Duration `json:"shutdown_flush_timeout"`
	ShutdownGracePeriod     time.Duration `json:"shutdown_grace_period"`
	ShutdownReportTimeout   time.Duration `json:"shutdown_report_timeout"`
	StaleHeartbeatAfter     time.Duration `json:"stale_heartbeat_after"`
	SummaryLimit            int           `json:"summary_limit"`
	PolicyRevision          string        `json:"policy_revision"`
	WarningRevision         string        `json:"warning_revision"`
}

type CLIAuth struct {
	VerificationURL          string        `json:"verification_url"`
	MachinesURL              string        `json:"machines_url"`
	ClientID                 string        `json:"client_id"`
	AllowedScopes            []string      `json:"allowed_scopes"`
	DeviceGrantLifetime      time.Duration `json:"device_grant_lifetime"`
	AccessTokenLifetime      time.Duration `json:"access_token_lifetime"`
	RefreshTokenLifetime     time.Duration `json:"refresh_token_lifetime"`
	PollInterval             time.Duration `json:"poll_interval"`
	MaxClientLabelLength     int           `json:"max_client_label_length"`
	NetworkRequestsPerMinute int           `json:"network_requests_per_minute"`
	GrantPollsPerMinute      int           `json:"grant_polls_per_minute"`
	AccountActionsPerMinute  int           `json:"account_actions_per_minute"`
	MintActiveKeyID          string        `json:"mint_active_key_id"`
	MintJWKSMaxAge           time.Duration `json:"mint_jwks_max_age"`
	// MintWithdrawnKeyIDs are signing keys no longer trusted: verification refuses them and the trust document
	// lists them in withdrawn_key_ids. The active key cannot be withdrawn.
	MintWithdrawnKeyIDs []string      `json:"mint_withdrawn_key_ids"`
	MintProofLifetime   time.Duration `json:"mint_proof_lifetime"`
	// ClientSessionTouchInterval throttles last_used_at writes for CLI client
	// sessions. Authenticated requests only rewrite the touch timestamp when
	// it is older than this interval, so hot request bursts do not pay a
	// write per request.
	ClientSessionTouchInterval time.Duration `json:"client_session_touch_interval"`
}

type GitHub struct {
	OAuthAuthorizeURL string   `json:"oauth_authorize_url"`
	OAuthTokenURL     string   `json:"oauth_token_url"`
	OAuthScopes       []string `json:"oauth_scopes"`
	AppID             string   `json:"app_id"`
	ConfigRepoName    string   `json:"config_repo_name"`
	ConfigRepoBranch  string   `json:"config_repo_branch"`
}

type Providers struct {
	WorkOS ProviderConfig `json:"workos"`
	Stripe ProviderConfig `json:"stripe"`
	GitHub ProviderConfig `json:"github"`
}

type ProviderConfig struct {
	BaseURL string `json:"base_url"`
}

type Access struct {
	RouteSubdomainPrefix string             `json:"route_subdomain_prefix,omitempty"`
	ConnectReadyTimeout  time.Duration      `json:"connect_ready_timeout,omitempty"`
	ConnectPollInterval  time.Duration      `json:"connect_poll_interval,omitempty"`
	FileTransfer         FileTransferPolicy `json:"file_transfer"`
}

type FileTransferPolicy struct {
	Revision               string        `json:"revision"`
	MaxBatchFiles          int           `json:"max_batch_files"`
	MaxConcurrentTransfers int           `json:"max_concurrent_transfers"`
	Retention              time.Duration `json:"retention"`
	DeliveryTimeout        time.Duration `json:"delivery_timeout"`
	MaxPendingSpoolBytes   int64         `json:"max_pending_spool_bytes"`
	// AutomaticMaxBatchBytes bounds one Automatic `pb send` (every file summed, so also any one
	// file); Unlimited has no bound but never uses a Paperboat-hosted relay. Published to clients.
	AutomaticMaxBatchBytes int64 `json:"automatic_max_batch_bytes"`
	// ModesMinimumVersion is the first official pb whose destination receives per the signed
	// transfer_mode/max_bytes claims (named in destination_update_required).
	ModesMinimumVersion string `json:"modes_minimum_version"`
}

type Diagnostics struct {
	ObjectEndpoint string        `json:"object_endpoint"`
	ObjectRegion   string        `json:"object_region"`
	ObjectBucket   string        `json:"object_bucket"`
	ForcePathStyle bool          `json:"force_path_style"`
	Retention      time.Duration `json:"retention"`
}

type Notifications struct {
	ReceiptEmailEndpoint string `json:"receipt_email_endpoint"`
	ReceiptEmailFrom     string `json:"receipt_email_from"`
}

// ReleaseAuthority holds only public verification material. The independent
// release authority keeps the threshold private signing keys outside this
// service and submits already signed policy bundles for audit and visibility.
type ReleaseAuthority struct {
	PublicKeys []string `json:"public_keys"`
	Threshold  int      `json:"threshold"`
}

type Secrets struct {
	ResendAPIKey          string   `json:"resend_api_key"`
	SentryAPIToken        string   `json:"sentry_api_token"`
	BackupReportToken     string   `json:"backup_report_token"`
	TestFixturesToken     string   `json:"test_fixtures_token"`
	SentryDSN             string   `json:"sentry_dsn"`
	SessionKeys           []string `json:"session_keys"`
	EncryptionKey         string   `json:"encryption_key"`
	WorkOSAPIKey          string   `json:"workos_api_key"`
	WorkOSClientID        string   `json:"workos_client_id"`
	WorkOSActionsSecret   string   `json:"workos_actions_secret"`
	StripeAPIKey          string   `json:"stripe_api_key"`
	StripeWebhookSecret   string   `json:"stripe_webhook_secret"`
	GitHubClientID        string   `json:"github_client_id"`
	GitHubClientSecret    string   `json:"github_client_secret"`
	EdgeControlCredential string   `json:"edge_control_credential"`
	MintSigningKeys       []string `json:"mint_signing_keys"`
	DiagnosticsAccessKey  string   `json:"diagnostics_access_key"`
	DiagnosticsSecretKey  string   `json:"diagnostics_secret_key"`
	ReceiptEmailToken     string   `json:"receipt_email_token"`
}

type LoadOptions struct {
	Environment string
	FilePath    string
	LookupEnv   func(string) (string, bool)
	ReadFile    func(string) ([]byte, error)
}

// DefaultSentryDSN and DefaultSentryRelease are populated only in official
// release artifacts. Source builds leave both empty and keep reporting off.
var (
	DefaultSentryDSN     string
	DefaultSentryRelease string
)

func Load(ctx context.Context, opts LoadOptions) (Config, error) {
	_ = ctx
	cfg := Default()
	if opts.LookupEnv == nil {
		opts.LookupEnv = os.LookupEnv
	}
	if opts.ReadFile == nil {
		opts.ReadFile = os.ReadFile
	}
	if opts.Environment != "" {
		cfg.Environment = Environment(opts.Environment)
	}
	if opts.FilePath != "" {
		b, err := opts.ReadFile(opts.FilePath)
		if err != nil {
			return Config{}, fmt.Errorf("read config file: %w", err)
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse config file: %w", err)
		}
	}
	if err := overlayEnv(&cfg, opts.LookupEnv, opts.ReadFile); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Default() Config {
	reportingEnabled := DefaultSentryDSN != "" && DefaultSentryRelease != ""
	return Config{
		Certificates:      Certificates{Challenge: CertificateChallengeDNS01},
		EdgeIngressLimits: edgelimits.Default(),
		Mail:              TransactionalMail{InvitationLifetimeSeconds: 2592000, InvitationMailDedupSeconds: 86400},
		Reporting: Reporting{
			Enabled: reportingEnabled,
			Release: DefaultSentryRelease,
		},
		Environment:     EnvironmentDevelopment,
		BrowserSessions: DefaultBrowserSessions(),
		HTTP: HTTPConfig{
			Address:           "127.0.0.1:8080",
			PublicBaseURL:     "http://127.0.0.1:8080",
			AllowedOrigins:    []string{"http://localhost:3000", "http://127.0.0.1:3000"},
			ReadHeaderTimeout: 5 * time.Second,
			RequestTimeout:    15 * time.Second,
			ShutdownTimeout:   10 * time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    64 << 10,
			MaxBodyBytes:      1 << 20,
		},
		Database: Database{
			Driver: "postgres",
			DSN:    "postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable",
		},
		RateLimits:        RateLimits{SignInPerMinute: 30, SignInBurst: 10, TokenFailuresPerMinute: 20, TokenFailuresBurst: 10, MachinePeerNetworkConfigPerMinute: 30, MachinePeerNetworkConfigBurst: 20, MaxClients: 100000},
		APITokens:         APITokens{MaxLifetimeDays: 365, DefaultLifetimeDays: 90, MaxPerMember: 100, RetrySeconds: 600},
		Billing:           Billing{TopupMaxQuantity: 100, TeamTrialDays: 14, PersonalAllowanceBytes: new(int64(5_000_000_000)), Limits: PlanLimits{Machines: PlanLimit{Free: 100, Pro: 1000, TeamCeiling: 10000}, Publications: PlanLimit{Free: 100, Pro: 1000, TeamCeiling: 10000}, BrowserTerminals: PlanLimit{Free: 10, Pro: 100, TeamCeiling: 1000}, ViewersPerSharedTerminal: 50, PublicTCP: PublicTCPLimit{Free: 1, Pro: 10, TeamPerSeat: 10}}},
		Business:          defaultBusiness(),
		WorkspacePolicy:   defaultWorkspacePolicy(),
		RuntimeBaseDomain: "localhost",
		ManagedSSH:        ManagedSSH{ClientKeyActiveWindow: 30 * 24 * time.Hour},
		UserMachines:      UserMachines{PairingLifetime: 10 * time.Minute, OfflineAfter: 2 * time.Minute, ExpiredAccessSessionRetention: 7 * 24 * time.Hour, AllowedPlatforms: []string{"darwin", "linux", "windows"}, RuntimeListenPort: 38080},
		TerminalSessions:  TerminalSessions{OperationTimeout: 15 * time.Second, RetryBackoff: time.Second, WorkerInterval: time.Second, MaxAttemptsBeforeAlert: 10, OperationMaxAge: 30 * 24 * time.Hour},
		Preview:           Preview{BaseDomain: "preview.localhost"},
		Tunnel:            Tunnel{BaseDomain: "tunnels.localhost"},
		ConfigSync: ConfigSync{
			Mode:             "disabled",
			ManifestContract: "paperboat-manifest-v1", ManifestMaxBytes: 256 << 10,
			ManifestMaxLines: 4096, ManifestMaxPatternBytes: 1024,
			MaxFileBytes: 5 << 20, MaxBatchBytes: 25 << 20,
			Debounce: 10 * time.Second, MinPushInterval: 5 * time.Minute, MaxDirtyDelay: 5 * time.Minute,
			RemotePollInterval: time.Minute, RetryLimit: 5, ShutdownFlushTimeout: 30 * time.Second,
			ShutdownGracePeriod: 2 * time.Second, ShutdownReportTimeout: 10 * time.Second,
			StaleHeartbeatAfter: 2 * time.Minute, SummaryLimit: 50, PolicyRevision: "1", WarningRevision: "config-sync-warning-v1",
		},
		CLIAuth: CLIAuth{
			VerificationURL:            "http://localhost:3000/cli/authorize",
			MachinesURL:                "http://localhost:3000/dashboard/machines",
			ClientID:                   "paperboat",
			AllowedScopes:              []string{"account:read", "clients:revoke", "machines:read", "machines:connect", "session:refresh", "diagnostics:upload", "previews:read", "previews:write", "tunnels:read", "tunnels:write", "operations:read", "operations:write"},
			DeviceGrantLifetime:        10 * time.Minute,
			AccessTokenLifetime:        15 * time.Minute,
			RefreshTokenLifetime:       30 * 24 * time.Hour,
			PollInterval:               5 * time.Second,
			MaxClientLabelLength:       120,
			NetworkRequestsPerMinute:   30,
			GrantPollsPerMinute:        30,
			AccountActionsPerMinute:    30,
			MintJWKSMaxAge:             time.Hour,
			MintProofLifetime:          admission.DefaultLifetime,
			ClientSessionTouchInterval: time.Minute,
		},
		Access: Access{
			RouteSubdomainPrefix: "pb",
			ConnectReadyTimeout:  2 * time.Second,
			ConnectPollInterval:  100 * time.Millisecond,
			FileTransfer: FileTransferPolicy{
				Revision: "file-transfer-v1", MaxBatchFiles: 10, AutomaticMaxBatchBytes: 2_000_000_000,
				MaxConcurrentTransfers: 2, Retention: 7 * 24 * time.Hour,
				DeliveryTimeout: 10 * time.Minute, MaxPendingSpoolBytes: 2_000_000_000,
			},
		},
		Diagnostics: Diagnostics{Retention: 7 * 24 * time.Hour},
		Retention:   defaultRetention(),
		Providers: Providers{
			WorkOS: ProviderConfig{BaseURL: "https://api.workos.com"},
			GitHub: ProviderConfig{
				BaseURL: "https://api.github.com",
			},
		},
		GitHub: GitHub{
			OAuthAuthorizeURL: "https://github.com/login/oauth/authorize",
			OAuthTokenURL:     "https://github.com/login/oauth/access_token",
			OAuthScopes:       []string{"repo"},
			ConfigRepoName:    "paperboat-config",
			ConfigRepoBranch:  "main",
		},
		Secrets: Secrets{
			SentryDSN:     DefaultSentryDSN,
			SessionKeys:   []string{"development-session-key-change-me"},
			EncryptionKey: "development-encryption-key-change-me",
		},
	}
}

func (c Config) Validate() error {
	if err := c.Retention.validate(c.UserMachines.ExpiredAccessSessionRetention); err != nil {
		return err
	}
	if err := c.BrowserSessions.Validate(); err != nil {
		return err
	}
	var errs []error
	if err := c.Mail.Validate(c.Secrets.ResendAPIKey); err != nil {
		errs = append(errs, err)
	}
	if c.Reporting.WorkerStallTimeout != 0 && (c.Reporting.WorkerStallTimeout < time.Minute || c.Reporting.WorkerStallTimeout > 24*time.Hour) {
		errs = append(errs, errors.New("reporting.worker_stall_timeout must be between one minute and 24 hours"))
	}
	if c.Reporting.HealthMonitor != "" && (!c.Reporting.Enabled || !regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`).MatchString(c.Reporting.HealthMonitor)) {
		errs = append(errs, errors.New("reporting.health_monitor requires enabled reporting and a valid public monitor slug"))
	}
	if (c.Reporting.Region != "" || c.Reporting.Instance != "") && (!regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`).MatchString(c.Reporting.Region) || !regexp.MustCompile(`^slot-(0[1-9]|[1-5][0-9]|6[0-4])$`).MatchString(c.Reporting.Instance)) {
		errs = append(errs, errors.New("reporting region and instance require a public region and slot-01 through slot-64"))
	}
	if rate := c.Reporting.TracesSampleRate; rate != nil && !(*rate >= 0 && *rate <= 1) {
		errs = append(errs, errors.New("reporting.traces_sample_rate must be between 0 and 1"))
	}
	if err := c.validateOperatorIntegrations(); err != nil {
		errs = append(errs, err)
	}
	if c.Reporting.Enabled {
		u, err := url.Parse(c.Secrets.SentryDSN)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User == nil || c.Reporting.Release == "" {
			errs = append(errs, errors.New("enabled reporting requires an HTTPS Sentry DSN and release"))
		}
	}
	if (c.Secrets.SentryDSN == "") != (c.Reporting.Release == "") {
		errs = append(errs, errors.New("reporting Sentry DSN and release must be configured together"))
	}
	if c.Reporting.IssueURL != "" {
		u, err := url.Parse(c.Reporting.IssueURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			errs = append(errs, errors.New("reporting.issue_url must be an HTTPS Sentry issues URL without credentials, query or fragment"))
		}
	}

	if c.Environment != EnvironmentDevelopment && c.Environment != EnvironmentTest && c.Environment != EnvironmentProduction {
		errs = append(errs, fmt.Errorf("environment must be development, test, or production"))
	}
	if _, _, err := net.SplitHostPort(c.HTTP.Address); err != nil {
		errs = append(errs, fmt.Errorf("http.address must be host:port: %w", err))
	}
	if c.HTTP.PublicBaseURL == "" {
		errs = append(errs, fmt.Errorf("http.public_base_url is required"))
	} else if _, err := url.ParseRequestURI(c.HTTP.PublicBaseURL); err != nil {
		errs = append(errs, fmt.Errorf("http.public_base_url must be a valid absolute URL"))
	}
	if c.HTTP.ReadHeaderTimeout <= 0 || c.HTTP.RequestTimeout <= 0 || c.HTTP.ShutdownTimeout <= 0 || c.HTTP.IdleTimeout <= 0 {
		errs = append(errs, fmt.Errorf("http timeouts must be positive"))
	}
	if c.HTTP.MaxHeaderBytes <= 0 {
		errs = append(errs, fmt.Errorf("http.max_header_bytes must be positive"))
	}
	if c.HTTP.BrowserLoginOrigin != "" {
		origin, e := url.Parse(c.HTTP.BrowserLoginOrigin)
		if e != nil || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || (origin.Scheme != "https" && (origin.Scheme != "http" || c.Environment == EnvironmentProduction)) {
			errs = append(errs, fmt.Errorf("http.browser_login_origin must be an exact trusted origin; production requires HTTPS"))
		}
	}
	if c.HTTP.MaxBodyBytes <= 0 {
		errs = append(errs, fmt.Errorf("http.max_body_bytes must be positive"))
	}
	for _, raw := range c.HTTP.TrustedProxyCIDRs {
		if _, _, err := net.ParseCIDR(strings.TrimSpace(raw)); err != nil {
			errs = append(errs, fmt.Errorf("http.trusted_proxy_cidrs contains invalid CIDR %q", raw))
		}
	}
	if c.Database.Driver == "" || c.Database.DSN == "" {
		errs = append(errs, fmt.Errorf("database.driver and database.dsn are required"))
	} else if c.Database.Driver != "postgres" && c.Database.Driver != "pgx" {
		errs = append(errs, fmt.Errorf("database.driver must be postgres"))
	}
	if err := c.WorkspacePolicy.validate(); err != nil {
		errs = append(errs, err)
	}
	if t := c.APITokens; t.MaxLifetimeDays < 1 || t.DefaultLifetimeDays < 1 || t.DefaultLifetimeDays > t.MaxLifetimeDays || t.MaxPerMember < 1 || t.RetrySeconds < 60 || t.RetrySeconds > 3600 {
		errs = append(errs, fmt.Errorf("api_tokens limits must be positive, default_lifetime_days at most max_lifetime_days and retry_seconds between 60 and 3600"))
	}
	if l := c.RateLimits; l.SignInPerMinute < 1 || l.SignInBurst < 1 || l.TokenFailuresPerMinute < 1 || l.TokenFailuresBurst < 1 || l.MachinePeerNetworkConfigPerMinute < 1 || l.MachinePeerNetworkConfigBurst < 1 || l.MaxClients < 1000 {
		errs = append(errs, errors.New("rate_limits must be positive and max_clients at least 1000"))
	}
	if c.Billing.TeamTrialDays < 0 || c.Billing.TeamTrialDays > 730 {
		return errors.New("billing.team_trial_days must be between 0 and 730")
	}
	if err := c.Business.validate(); err != nil {
		errs = append(errs, err)
	}
	if c.Billing.TopupMaxQuantity < 1 {
		return errors.New("billing.topup_max_quantity must be positive")
	}
	if (c.Billing.PersonalAllowanceBytes != nil && *c.Billing.PersonalAllowanceBytes < 0) || (c.Billing.TeamAllowanceBytes != nil && *c.Billing.TeamAllowanceBytes < 0) {
		errs = append(errs, fmt.Errorf("configured billing allowances must be nonnegative"))
	}
	if l := c.Billing.Limits; !l.Machines.valid() || !l.Publications.valid() || !l.BrowserTerminals.valid() || l.ViewersPerSharedTerminal < 1 || l.PublicTCP.Free < 1 || l.PublicTCP.Pro < l.PublicTCP.Free || l.PublicTCP.TeamPerSeat < 1 {
		errs = append(errs, fmt.Errorf("billing.limits: each plan limit needs free > 0, pro >= free and team_ceiling >= pro, and viewers_per_shared_terminal > 0"))
	}
	if c.UserMachines.ExpiredAccessSessionRetention < time.Hour {
		errs = append(errs, fmt.Errorf("user_machines expired access session retention must be at least one hour"))
	}
	if c.ManagedSSH.ClientKeyActiveWindow < time.Hour {
		errs = append(errs, fmt.Errorf("managed_ssh client key active window must be at least one hour"))
	}
	if c.UserMachines.PairingLifetime <= 0 || c.UserMachines.OfflineAfter <= 0 || len(c.UserMachines.AllowedPlatforms) == 0 {
		errs = append(errs, fmt.Errorf("user_machines pairing lifetime, offline timeout, and allowed platforms are required"))
	} else {
		for _, platform := range c.UserMachines.AllowedPlatforms {
			if platform != "darwin" && platform != "linux" && platform != "windows" {
				errs = append(errs, fmt.Errorf("user_machines allowed platform %q is unsupported", platform))
			}
		}
	}
	if err := c.EdgeIngressLimits.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("edge_ingress_limits: %w", err))
	}
	if c.Environment == EnvironmentProduction && c.SelfhostAllowPrivateIngress {
		errs = append(errs, errors.New("selfhost_allow_private_ingress (PAPERBOAT_SELFHOST_ALLOW_PRIVATE_INGRESS) is for staging only and is refused in production"))
	}
	if c.Environment == EnvironmentProduction {
		publicURL, publicURLErr := url.Parse(c.HTTP.PublicBaseURL)
		if publicURLErr != nil || publicURL.Scheme != "https" || publicURL.User != nil || publicURL.Hostname() == "" || (publicURL.Path != "" && publicURL.Path != "/") || publicURL.RawQuery != "" || publicURL.Fragment != "" {
			errs = append(errs, fmt.Errorf("http.public_base_url must be an HTTPS origin in production"))
		}
		if strings.TrimSpace(c.ReleaseDirectory) == "" || !filepath.IsAbs(c.ReleaseDirectory) || strings.TrimSpace(c.RuntimeBaseDomain) == "" || c.UserMachines.RuntimeListenPort < 1024 {
			errs = append(errs, fmt.Errorf("release directory and user-machine runtime route are required in production"))
		}
		if strings.TrimSpace(c.ReleaseBaseURL) == "" {
			errs = append(errs, fmt.Errorf("release_base_url is required in production"))
		}
		if strings.TrimSpace(c.Preview.BaseDomain) == "" || strings.TrimSpace(c.Tunnel.BaseDomain) == "" {
			errs = append(errs, fmt.Errorf("preview and tunnel base domains are required in production"))
		}
	}
	if strings.TrimSpace(c.ReleaseBaseURL) != "" {
		releaseURL, releaseURLErr := url.Parse(c.ReleaseBaseURL)
		if releaseURLErr != nil || releaseURL.Scheme != "https" || releaseURL.User != nil || releaseURL.Hostname() == "" || (releaseURL.Path != "" && releaseURL.Path != "/") || releaseURL.RawQuery != "" || releaseURL.Fragment != "" {
			errs = append(errs, fmt.Errorf("release_base_url must be an HTTPS origin"))
		}
	}
	if len(c.ReleaseAuthority.PublicKeys) == 0 {
		if c.ReleaseAuthority.Threshold != 0 {
			errs = append(errs, fmt.Errorf("release_authority.threshold requires public keys"))
		}
	} else if c.ReleaseAuthority.Threshold < 2 || c.ReleaseAuthority.Threshold > len(c.ReleaseAuthority.PublicKeys) || len(c.ReleaseAuthority.PublicKeys) > 16 {
		errs = append(errs, fmt.Errorf("release_authority.threshold must be between 2 and the number of public keys"))
	}
	if c.TerminalSessions.OperationTimeout <= 0 || c.TerminalSessions.RetryBackoff <= 0 || c.TerminalSessions.WorkerInterval <= 0 || c.TerminalSessions.MaxAttemptsBeforeAlert <= 0 || c.TerminalSessions.OperationMaxAge < time.Hour {
		errs = append(errs, fmt.Errorf("terminal_sessions limits and timings must be positive"))
	}

	if c.ConfigSync.MaxFileBytes < 1 || c.ConfigSync.MaxFileBytes > 100<<20 ||
		c.ConfigSync.MaxBatchBytes < c.ConfigSync.MaxFileBytes || c.ConfigSync.MaxBatchBytes > 500<<20 ||
		c.ConfigSync.ManifestContract != "paperboat-manifest-v1" ||
		c.ConfigSync.ManifestMaxBytes < 1 || c.ConfigSync.ManifestMaxBytes > 4<<20 ||
		c.ConfigSync.ManifestMaxLines < 1 || c.ConfigSync.ManifestMaxLines > 65536 ||
		c.ConfigSync.ManifestMaxPatternBytes < 1 || c.ConfigSync.ManifestMaxPatternBytes > 8192 {
		errs = append(errs, fmt.Errorf("config_sync manifest and size limits are invalid"))
	}
	if c.ConfigSync.Mode != "disabled" && c.ConfigSync.Mode != "read_only" && c.ConfigSync.Mode != "leased_writes" {
		errs = append(errs, fmt.Errorf("config_sync.mode must be disabled, read_only, or leased_writes"))
	}
	if strings.TrimSpace(c.ConfigSync.HomeOverride) != "" && !filepath.IsAbs(c.ConfigSync.HomeOverride) {
		errs = append(errs, fmt.Errorf("config_sync.home_override must be an absolute path"))
	}
	if c.ConfigSync.Debounce < time.Second || c.ConfigSync.Debounce > 5*time.Minute ||
		c.ConfigSync.MinPushInterval < time.Minute || c.ConfigSync.MinPushInterval > 24*time.Hour ||
		c.ConfigSync.MaxDirtyDelay < c.ConfigSync.Debounce || c.ConfigSync.MaxDirtyDelay > 24*time.Hour ||
		c.ConfigSync.RemotePollInterval < time.Second || c.ConfigSync.RemotePollInterval > time.Hour ||
		c.ConfigSync.RetryLimit < 1 || c.ConfigSync.RetryLimit > 20 ||
		c.ConfigSync.ShutdownFlushTimeout < time.Second || c.ConfigSync.ShutdownFlushTimeout > 10*time.Minute ||
		c.ConfigSync.ShutdownGracePeriod <= 0 || c.ConfigSync.ShutdownReportTimeout <= 0 ||
		c.ConfigSync.StaleHeartbeatAfter <= 0 || c.ConfigSync.SummaryLimit < 1 || c.ConfigSync.SummaryLimit > 1000 ||
		strings.TrimSpace(c.ConfigSync.PolicyRevision) == "" || strings.TrimSpace(c.ConfigSync.WarningRevision) == "" {
		errs = append(errs, fmt.Errorf("config_sync timing, retention, and policy revision are required"))
	}
	if strings.TrimSpace(c.CLIAuth.VerificationURL) == "" || strings.TrimSpace(c.CLIAuth.MachinesURL) == "" || strings.TrimSpace(c.CLIAuth.ClientID) == "" || len(c.CLIAuth.AllowedScopes) == 0 {
		errs = append(errs, fmt.Errorf("cli_auth verification_url, machines_url, client_id, and allowed_scopes are required"))
	}
	for _, requiredScope := range []string{"machines:read", "machines:connect", "previews:read", "previews:write", "tunnels:read", "tunnels:write", "operations:read", "operations:write"} {
		found := false
		for _, configuredScope := range c.CLIAuth.AllowedScopes {
			if configuredScope == requiredScope {
				found = true
				break
			}
		}
		if !found {
			errs = append(errs, fmt.Errorf("cli_auth.allowed_scopes must include %s for one-shot CLI enrollment", requiredScope))
		}
	}
	if verificationURL, err := url.Parse(c.CLIAuth.VerificationURL); err != nil || (verificationURL.Scheme != "http" && verificationURL.Scheme != "https") || verificationURL.Host == "" {
		errs = append(errs, fmt.Errorf("cli_auth.verification_url must be an absolute http or https URL"))
	}
	if machinesURL, err := url.Parse(c.CLIAuth.MachinesURL); err != nil || (machinesURL.Scheme != "http" && machinesURL.Scheme != "https") || machinesURL.Host == "" {
		errs = append(errs, fmt.Errorf("cli_auth.machines_url must be an absolute http or https URL"))
	}
	if c.CLIAuth.DeviceGrantLifetime <= 0 || c.CLIAuth.AccessTokenLifetime <= 0 || c.CLIAuth.RefreshTokenLifetime <= 0 || c.CLIAuth.PollInterval <= 0 || c.CLIAuth.ClientSessionTouchInterval <= 0 {
		errs = append(errs, fmt.Errorf("cli_auth lifetimes and poll_interval must be positive"))
	}
	if c.CLIAuth.MaxClientLabelLength <= 0 || c.CLIAuth.NetworkRequestsPerMinute <= 0 || c.CLIAuth.GrantPollsPerMinute <= 0 || c.CLIAuth.AccountActionsPerMinute <= 0 {
		errs = append(errs, fmt.Errorf("cli_auth limits must be positive"))
	}
	if c.CLIAuth.MintJWKSMaxAge <= 0 {
		errs = append(errs, fmt.Errorf("cli_auth.mint_jwks_max_age must be positive"))
	}
	if c.CLIAuth.MintProofLifetime <= 0 || c.CLIAuth.MintProofLifetime > 5*time.Minute {
		errs = append(errs, fmt.Errorf("cli_auth.mint_proof_lifetime must be positive and at most five minutes"))
	}
	if strings.TrimSpace(c.Access.RouteSubdomainPrefix) == "" {
		errs = append(errs, fmt.Errorf("access.route_subdomain_prefix is required"))
	}
	if c.Access.ConnectReadyTimeout <= 0 {
		errs = append(errs, fmt.Errorf("access.connect_ready_timeout must be positive"))
	}
	if c.Access.ConnectPollInterval <= 0 || c.Access.ConnectPollInterval > c.Access.ConnectReadyTimeout {
		errs = append(errs, fmt.Errorf("access.connect_poll_interval must be positive and no greater than connect_ready_timeout"))
	}
	transfer := c.Access.FileTransfer
	if strings.TrimSpace(transfer.Revision) == "" || transfer.AutomaticMaxBatchBytes < 1 || transfer.AutomaticMaxBatchBytes > 1<<40 || transfer.MaxBatchFiles < 1 || transfer.MaxBatchFiles > 10 || transfer.MaxConcurrentTransfers < 1 || transfer.MaxConcurrentTransfers > 2 || transfer.Retention <= 0 || transfer.DeliveryTimeout <= 0 || transfer.MaxPendingSpoolBytes < transfer.AutomaticMaxBatchBytes {
		errs = append(errs, fmt.Errorf("access.file_transfer policy is invalid"))
	}
	diagnosticsConfigured := strings.TrimSpace(c.Diagnostics.ObjectEndpoint) != "" || strings.TrimSpace(c.Diagnostics.ObjectRegion) != "" || strings.TrimSpace(c.Diagnostics.ObjectBucket) != "" || strings.TrimSpace(c.Secrets.DiagnosticsAccessKey) != "" || strings.TrimSpace(c.Secrets.DiagnosticsSecretKey) != ""
	if diagnosticsConfigured {
		endpoint, err := url.Parse(c.Diagnostics.ObjectEndpoint)
		if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "" || endpoint.Scheme != "https" && !(c.Environment != EnvironmentProduction && endpoint.Scheme == "http") {
			errs = append(errs, fmt.Errorf("diagnostics.object_endpoint must be an HTTPS origin"))
		}
		if !validStorageName(c.Diagnostics.ObjectRegion, 1, 128) || !validStorageName(c.Diagnostics.ObjectBucket, 3, 63) || c.Diagnostics.Retention < time.Hour || c.Diagnostics.Retention > 30*24*time.Hour || len(c.Secrets.DiagnosticsAccessKey) < 3 || len(c.Secrets.DiagnosticsSecretKey) < 8 {
			errs = append(errs, fmt.Errorf("diagnostics object storage configuration is incomplete or invalid"))
		}
	} else if c.Environment == EnvironmentProduction {
		errs = append(errs, fmt.Errorf("diagnostics object storage is required in production"))
	}
	receiptEmailConfigured := strings.TrimSpace(c.Notifications.ReceiptEmailEndpoint) != "" || strings.TrimSpace(c.Notifications.ReceiptEmailFrom) != "" || strings.TrimSpace(c.Secrets.ReceiptEmailToken) != ""
	if receiptEmailConfigured {
		endpoint, err := url.Parse(c.Notifications.ReceiptEmailEndpoint)
		if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Scheme != "https" && !(c.Environment != EnvironmentProduction && endpoint.Scheme == "http") || strings.TrimSpace(c.Notifications.ReceiptEmailFrom) == "" || strings.ContainsAny(c.Notifications.ReceiptEmailFrom, "\r\n\x00") || len(c.Secrets.ReceiptEmailToken) < 8 {
			errs = append(errs, fmt.Errorf("receipt email delivery configuration is incomplete or invalid"))
		}
	}
	if strings.TrimSpace(c.GitHub.OAuthAuthorizeURL) == "" || strings.TrimSpace(c.GitHub.OAuthTokenURL) == "" {
		errs = append(errs, fmt.Errorf("github oauth urls are required"))
	}
	if len(c.GitHub.OAuthScopes) == 0 {
		errs = append(errs, fmt.Errorf("github.oauth_scopes is required"))
	}
	if strings.TrimSpace(c.GitHub.ConfigRepoName) == "" || strings.TrimSpace(c.GitHub.ConfigRepoBranch) == "" {
		errs = append(errs, fmt.Errorf("github config repo name and branch are required"))
	}
	if runtimeDomain, err := url.Parse("https://" + strings.TrimSpace(c.RuntimeBaseDomain)); err != nil || runtimeDomain.Hostname() != strings.TrimSpace(c.RuntimeBaseDomain) || runtimeDomain.Port() != "" {
		errs = append(errs, fmt.Errorf("runtime_base_domain must be a DNS hostname"))
	}
	if (c.LocalAccessBaseDomain != "" || c.Environment == EnvironmentProduction) && !validEndpointBaseDomain(c.LocalAccessBaseDomain) {
		errs = append(errs, fmt.Errorf("local_access_base_domain must be a DNS hostname"))
	}
	if !validEndpointBaseDomain(c.Preview.BaseDomain) {
		errs = append(errs, fmt.Errorf("preview.base_domain must be a DNS hostname"))
	}
	if !validEndpointBaseDomain(c.Tunnel.BaseDomain) {
		errs = append(errs, fmt.Errorf("tunnel.base_domain must be a DNS hostname"))
	}
	if err := c.Certificates.Validate(c.Environment); err != nil {
		errs = append(errs, err)
	}
	if len(c.Secrets.SessionKeys) == 0 || c.Secrets.EncryptionKey == "" {
		errs = append(errs, fmt.Errorf("session and encryption secrets are required"))
	}
	if c.Secrets.EdgeControlCredential != "" && len(c.Secrets.EdgeControlCredential) < 32 {
		errs = append(errs, fmt.Errorf("edge control credential must be at least 32 characters"))
	}
	workOSClientID := strings.TrimSpace(c.Secrets.WorkOSClientID)
	workOSAPIKey := strings.TrimSpace(c.Secrets.WorkOSAPIKey)
	if (workOSClientID == "") != (workOSAPIKey == "") {
		errs = append(errs, errors.New("WorkOS client ID and API key must be configured together"))
	}
	if workOSClientID != "" && workOSAPIKey != "" {
		providerURL, err := url.Parse(c.Providers.WorkOS.BaseURL)
		if err != nil || providerURL.Scheme != "https" || !strings.EqualFold(providerURL.Host, "api.workos.com") || providerURL.Path != "" && providerURL.Path != "/" || providerURL.RawQuery != "" || providerURL.Fragment != "" || providerURL.User != nil {
			errs = append(errs, errors.New("WorkOS base URL must be https://api.workos.com"))
		}
	}
	if c.Environment == EnvironmentProduction {
		if !c.Certificates.Enabled {
			errs = append(errs, fmt.Errorf("managed certificates are required in production"))
		}
		if len(c.HTTP.AllowedOrigins) == 0 {
			errs = append(errs, fmt.Errorf("http.allowed_origins is required in production"))
		}
		if c.Secrets.WorkOSAPIKey == "" || c.Secrets.WorkOSClientID == "" || c.Secrets.GitHubClientID == "" || c.Secrets.GitHubClientSecret == "" {
			errs = append(errs, fmt.Errorf("production provider secrets are required"))
		}
		if len(c.Secrets.EdgeControlCredential) < 32 {
			errs = append(errs, fmt.Errorf("production edge control credential is required"))
		}
		if strings.TrimSpace(c.CLIAuth.MintActiveKeyID) == "" || len(c.Secrets.MintSigningKeys) == 0 {
			errs = append(errs, fmt.Errorf("production mint active key id and signing keys are required"))
		}
		for _, secret := range append(c.Secrets.SessionKeys, c.Secrets.EncryptionKey) {
			if strings.Contains(secret, "development") || len(secret) < 32 {
				errs = append(errs, fmt.Errorf("production secrets must be strong and non-development"))
				break
			}
		}
	}
	return errors.Join(errs...)
}

func (c Certificates) Validate(environment Environment) error {
	if !c.Enabled {
		return nil
	}
	var errs []error
	if strings.TrimSpace(c.DirectoryURL) == "" {
		errs = append(errs, fmt.Errorf("certificates.directory_url is required when certificates are enabled"))
	} else if parsed, err := url.Parse(c.DirectoryURL); err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Scheme != "https" && !(environment != EnvironmentProduction && parsed.Scheme == "http" && net.ParseIP(parsed.Hostname()) != nil && net.ParseIP(parsed.Hostname()).IsLoopback()) {
		errs = append(errs, fmt.Errorf("certificates.directory_url must be HTTPS (or loopback HTTP outside production)"))
	}
	for name, value := range map[string]string{
		"account_key_reference":             c.AccountKeyReference,
		"master_key_reference":              c.MasterKeyReference,
		"distribution_credential_reference": c.DistributionCredentialReference,
		"owner_id":                          c.OwnerID,
	} {
		if !validCertificateReference(value) {
			errs = append(errs, fmt.Errorf("certificates.%s is required and must be a safe reference", name))
		}
	}
	if c.ChallengeType() == CertificateChallengeDNS01 && !validCertificateReference(c.DNSTokenReference) {
		errs = append(errs, fmt.Errorf("certificates.dns_token_reference is required and must be a safe reference"))
	}
	switch c.ChallengeType() {
	case CertificateChallengeDNS01:
		errs = append(errs, c.validateDNS01()...)
	case CertificateChallengeHTTP01:
		if environment == EnvironmentProduction {
			errs = append(errs, fmt.Errorf("certificates.challenge %s (PAPERBOAT_CERTIFICATES_CHALLENGE) is for staging only and is refused in production", CertificateChallengeHTTP01))
		}
	default:
		errs = append(errs, fmt.Errorf("certificates.challenge must be %s or %s", CertificateChallengeDNS01, CertificateChallengeHTTP01))
	}
	if host, port, err := net.SplitHostPort(strings.TrimSpace(c.CAAResolver)); err != nil || host == "" || port == "" {
		errs = append(errs, fmt.Errorf("certificates.caa_resolver must be host:port"))
	}
	if strings.TrimSpace(c.Issuer) == "" || !validConfigMetadata(c.Issuer, 128) {
		errs = append(errs, fmt.Errorf("certificates.issuer is required"))
	}
	if c.AccountEmail != "" && (strings.TrimSpace(c.AccountEmail) != c.AccountEmail || strings.ContainsAny(c.AccountEmail, "\r\n\x00") || len(c.AccountEmail) > 256) {
		errs = append(errs, fmt.Errorf("certificates.account_email is invalid"))
	}
	if c.RenewBefore < 0 || c.RenewBefore >= 365*24*time.Hour || c.LockTTL < 0 || c.LockTTL > 15*time.Minute || c.DistributionTimeout < 0 || c.DistributionTimeout > 2*time.Minute || c.ExpiryAlertWindow < 0 || c.ExpiryAlertWindow > 90*24*time.Hour || c.MaxCertificateLifetime < 0 || c.MaxCertificateLifetime > 90*24*time.Hour || c.ACMETimeout < 0 || c.ACMETimeout > 10*time.Minute || c.PropagationTimeout < 0 || c.PropagationTimeout > 10*time.Minute || c.CleanupTimeout < 0 || c.CleanupTimeout > 2*time.Minute || c.PollInterval < 0 || c.PollInterval > time.Minute || c.RetryBase < 0 || c.RetryBase > time.Minute || c.RetryMax < 0 || c.RetryMax > 2*time.Minute || c.MaxAttempts < 0 || c.MaxAttempts > 8 {
		errs = append(errs, fmt.Errorf("certificates timing and retry bounds are invalid"))
	}
	return errors.Join(errs...)
}

// CertificateChallengeDNS01 and CertificateChallengeHTTP01 are the only ACME
// challenge types. http-01 is a staging path: it is refused in production.
const (
	CertificateChallengeDNS01  = "dns-01"
	CertificateChallengeHTTP01 = "http-01"
)

// ChallengeType is the ACME challenge in use: the configured one, or dns-01 when none is set, so a configuration
// written before the switch (or a test fixture) keeps its meaning.
func (c Certificates) ChallengeType() string {
	if strings.TrimSpace(c.Challenge) == "" {
		return CertificateChallengeDNS01
	}
	return c.Challenge
}

// validateDNS01 checks the Cloudflare zones and challenge delegation that only
// the DNS-01 challenge needs.
func (c Certificates) validateDNS01() []error {
	var errs []error
	if strings.TrimSpace(c.DNSProvider) != "cloudflare" {
		errs = append(errs, fmt.Errorf("certificates.dns_provider must be cloudflare"))
	}
	if !validConfigMetadata(c.DNSZoneID, 128) {
		errs = append(errs, fmt.Errorf("certificates.dns_zone_id is required"))
	}
	if !validConfigMetadata(c.PublicDNSZoneID, 128) {
		errs = append(errs, fmt.Errorf("certificates.public_dns_zone_id is required"))
	}
	if !validEndpointBaseDomain(c.ChallengeZone) {
		errs = append(errs, fmt.Errorf("certificates.challenge_zone must be a DNS hostname"))
	}
	if c.DNSWriteBase != "" && !validEndpointBaseDomain(c.DNSWriteBase) {
		errs = append(errs, fmt.Errorf("certificates.dns_write_base must be a DNS hostname"))
	}
	return errs
}

func validCertificateReference(value string) bool {
	if !validConfigMetadata(value, 256) {
		return false
	}
	if strings.Contains(strings.ToLower(value), "begin ") || strings.Contains(strings.ToLower(value), "private key") {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.' || character == ':' || character == '/') {
			return false
		}
	}
	return true
}

func validConfigMetadata(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n\x00")
}

func validStorageName(value string, minimum, maximum int) bool {
	value = strings.TrimSpace(value)
	if len(value) < minimum || len(value) > maximum {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '.' || character == '_') {
			return false
		}
	}
	return true
}

func splitCSV(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func validEndpointBaseDomain(raw string) bool {
	domain := strings.TrimSpace(raw)
	if domain == "" || domain != raw || domain != strings.ToLower(domain) || len(domain) > 253 || strings.HasSuffix(domain, ".") {
		return false
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return false
			}
		}
	}
	return true
}

// Published is the file transfer policy as clients receive it (descriptor and connector admission).
func (p FileTransferPolicy) Published() mint.FileTransferPolicy {
	return mint.FileTransferPolicy{
		Revision: p.Revision, MaxBatchFiles: p.MaxBatchFiles, AutomaticMaxBatchBytes: p.AutomaticMaxBatchBytes,
		MaxConcurrentTransfers: p.MaxConcurrentTransfers, RetentionSeconds: int64(p.Retention / time.Second),
		DeliveryTimeoutSeconds: int64(p.DeliveryTimeout / time.Second), MaxPendingSpoolBytes: p.MaxPendingSpoolBytes,
	}
}
