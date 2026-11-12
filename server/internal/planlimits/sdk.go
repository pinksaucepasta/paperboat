package planlimits

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/ratelimit"
)

// SDKLimitError means no session or operation was created by this request.
type SDKLimitError struct {
	Kind       string
	RetryAfter time.Duration
}

func (e *SDKLimitError) Error() string {
	return fmt.Sprintf("SDK %s limit reached; wait or close an active session/operation", e.Kind)
}

type sdkRates struct{ sessions, descriptors *ratelimit.Limiter }

var sdkRatePolicy atomic.Pointer[sdkRates]

func init() { ConfigureSDKRates(config.Default().RateLimits) }

func ConfigureSDKRates(c config.RateLimits) {
	sdkRatePolicy.Store(&sdkRates{sessions: ratelimit.New(c.SDKSessionPerMinute, c.SDKSessionBurst, c.MaxClients), descriptors: ratelimit.New(c.SDKDescriptorPerMinute, c.SDKDescriptorBurst, c.MaxClients)})
}

// AllowSDKRequest uses the existing bounded buckets for both token and creator, before database work.
func AllowSDKRequest(account, token, kind string) error {
	rates := sdkRatePolicy.Load()
	limiter := rates.descriptors
	if kind == "sessions" {
		limiter = rates.sessions
	}
	for _, key := range []string{"account:" + account, "token:" + token} {
		if allowed, retry := limiter.Allow(key); !allowed {
			return &SDKLimitError{Kind: kind, RetryAfter: retry}
		}
	}
	return nil
}

// SDKRow is shared by pgx and database/sql transaction adapters.
type SDKRow interface{ Scan(...any) error }

// CheckSDKCount serializes reservations with their insert transaction. An operation holds its reservation
// while its admission is live (expires_at, minutes) until a signed destination outcome or a revocation
// releases it early. Without the expiry bound, an operation no destination ever ends (a terminal, a client
// that died before connecting) held its slot until the token was revoked, and ~10 runs filled a token's
// cap for good (2026-11-12). A semaphore lease that expires unless renewed, as Teleport's
// lib/services/semaphore.go does; nothing renews an admission.
func CheckSDKCount(ctx context.Context, query func(context.Context, string, ...any) SDKRow, account, token, kind string) error {
	var locked any
	if err := query(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,7311))`, "sdk:"+account+":"+kind).Scan(&locked); err != nil {
		return err
	}
	limits := current.Load().limits.SDK
	perToken, perAccount := limits.SessionsPerToken, limits.SessionsPerAccount
	from := `FROM paperboat.cli_client_sessions c JOIN paperboat.api_tokens t ON t.id=c.api_token_id WHERE c.user_id=$1 AND c.state='active' AND c.revoked_at IS NULL AND t.revoked_at IS NULL AND t.expires_at>now()`
	if kind == "operations" {
		perToken, perAccount = limits.OperationsPerToken, limits.OperationsPerAccount
		from = `FROM paperboat.user_machine_access_sessions a JOIN paperboat.cli_client_sessions c ON c.id=a.cli_client_session_id AND c.user_id=a.user_id JOIN paperboat.api_tokens t ON t.id=c.api_token_id WHERE a.user_id=$1 AND a.state='active' AND a.revoked_at IS NULL AND a.expires_at>now() AND c.state='active' AND c.revoked_at IS NULL`
	}
	var tokenCount, accountCount int
	if err := query(ctx, `SELECT count(*) FILTER(WHERE t.id=$2),count(*) `+from, account, token).Scan(&tokenCount, &accountCount); err != nil {
		return err
	}
	if tokenCount >= perToken || accountCount >= perAccount {
		return &SDKLimitError{Kind: kind, RetryAfter: time.Second}
	}
	return nil
}
