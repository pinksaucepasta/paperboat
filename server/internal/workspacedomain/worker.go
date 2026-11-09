package workspacedomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/miekg/dns"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
	"github.com/pinksaucepasta/paperboat/server/internal/workers"
)

const (
	dnsRetryMin      = 30 * time.Second
	dnsRetryMax      = 30 * time.Minute
	dnsVerifiedEvery = time.Hour
	// A domain that stays unproven is checked at the slowest pace until the user asks again (Check).
	dnsFailedAfter = 8
	// The domain renews before the hostnames' copies come due (their RenewBefore), so a copy's renewal
	// finds a newer certificate waiting instead of retrying.
	renewLead = 5 * 24 * time.Hour
)

// CNAMELookup returns the CNAME target of name (lowercase, no trailing dot), or "" when name has none.
type CNAMELookup func(ctx context.Context, name string) (string, error)

// NamesIssuer orders one certificate for several names (tunnelcert.ACMEIssuer).
type NamesIssuer interface {
	IssueDNS01Names(context.Context, tunnelcert.Domain, []string) (tunnelcert.CertificateBundle, error)
}

type WorkerConfig struct {
	Lookup             CNAMELookup // nil: the system resolvers
	Issuer             NamesIssuer // nil: certificates are not issued (http-01 deployments cannot issue wildcards)
	CAA                tunnelcert.CAAInspector
	Keys               tunnelcert.MasterKeySource
	MasterKeyReference string
	IssuerName         string
	OwnerID            string
	RenewBefore        time.Duration
	LockTTL            time.Duration
	MaxLifetime        time.Duration
}

// Worker checks workspace domains' verification CNAMEs and issues their wildcard certificates. It also
// serves those certificates to the hostnames under each domain (tunnelcert.WorkspaceCertificateSource).
type Worker struct {
	db    *db.DB
	cfg   Config
	wc    WorkerConfig
	locks *tunnelcert.SQLIssuanceLock
	now   func() time.Time
}

func NewWorker(database *db.DB, cfg Config, wc WorkerConfig) (*Worker, error) {
	locks, err := tunnelcert.NewSQLIssuanceLock(database)
	if err != nil {
		return nil, err
	}
	if wc.Lookup == nil {
		wc.Lookup = SystemCNAME
	}
	if wc.RenewBefore <= 0 {
		wc.RenewBefore = 30 * 24 * time.Hour
	}
	if wc.LockTTL <= 0 {
		wc.LockTTL = 10 * time.Minute
	}
	if wc.IssuerName == "" {
		wc.IssuerName = "letsencrypt"
	}
	return &Worker{db: database, cfg: cfg, wc: wc, locks: locks, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Run is one pass: due DNS checks, then due certificates.
func (w *Worker) Run(ctx context.Context, limit int) error {
	now := w.now()
	rows, err := w.db.Queries().ListDueWorkspaceDomainDNS(ctx, dbsqlc.ListDueWorkspaceDomainDNSParams{Now: now, RowLimit: int32(limit)})
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := w.checkDNS(ctx, row, now); err != nil {
			return err
		}
	}
	if w.wc.Issuer == nil {
		return nil
	}
	due, err := w.db.Queries().ListDueWorkspaceDomainCertificates(ctx, dbsqlc.ListDueWorkspaceDomainCertificatesParams{
		Now: sql.NullTime{Time: now, Valid: true}, RenewAfter: sql.NullTime{Time: now.Add(w.wc.RenewBefore + renewLead), Valid: true}, RowLimit: int32(limit)})
	if err != nil {
		return err
	}
	for _, row := range due {
		if err := w.issue(ctx, row); err != nil {
			code := certificateFailureCode(err)
			retry := now.Add(certificateRetry(row.CertificateAttempts + 1))
			if markErr := w.db.Queries().MarkWorkspaceDomainCertificateFailure(ctx, dbsqlc.MarkWorkspaceDomainCertificateFailureParams{
				ID: row.ID, FailureCode: sql.NullString{String: code, Valid: true}, RetryAt: sql.NullTime{Time: retry, Valid: true}, Now: w.now()}); markErr != nil {
				return errors.Join(err, markErr)
			}
			workers.IterationFailure(ctx, "workspace_domain_certificate", code, err)
		}
	}
	return nil
}

func (w *Worker) checkDNS(ctx context.Context, row dbsqlc.WorkspaceDomain, now time.Time) error {
	want, err := tunnelcert.WorkspaceChallengeTarget(row.ChallengeReference, row.Hostname, w.cfg.ChallengeZone)
	if err != nil {
		return err
	}
	name := "_acme-challenge." + row.Hostname
	lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	got, lookupErr := w.wc.Lookup(lookupCtx, name)
	cancel()
	state, code, observed := "pending", "", []string{}
	switch {
	case lookupErr != nil:
		code = "dns_unavailable"
		state = row.OwnershipState // a resolver failure proves nothing either way
	case got == "":
		code = "dns_not_found"
	case got != want:
		code = "wrong_record"
		observed = []string{name + " CNAME " + got}
	default:
		state, observed = "verified", []string{name + " CNAME " + got}
	}
	if state == "pending" && row.VerificationAttempts+1 >= dnsFailedAfter {
		state = "failed"
	}
	next := now.Add(dnsVerifiedEvery)
	if state != "verified" {
		next = now.Add(dnsBackoff(row.VerificationAttempts))
	}
	raw, err := json.Marshal(observed)
	if err != nil {
		return err
	}
	return w.db.Queries().ApplyWorkspaceDomainDNSObservation(ctx, dbsqlc.ApplyWorkspaceDomainDNSObservationParams{
		ID: row.ID, OwnershipState: state, DnsFailureCode: sql.NullString{String: code, Valid: code != ""},
		ObservedRecords: raw, Now: sql.NullTime{Time: now, Valid: true}, NextCheckAt: next})
}

func dnsBackoff(attempts int32) time.Duration {
	delay := dnsRetryMin
	for i := int32(0); i < attempts && delay < dnsRetryMax; i++ {
		delay *= 2
	}
	return min(delay, dnsRetryMax)
}

func certificateRetry(attempts int32) time.Duration {
	delay := time.Minute
	for i := int32(1); i < attempts && delay < 6*time.Hour; i++ {
		delay *= 2
	}
	return min(delay, 6*time.Hour)
}

// issue orders the domain's certificate for the domain and *.domain through the delegated _acme-challenge
// CNAME (the record that verified it), seals it with the certificate master key and stores it.
func (w *Worker) issue(ctx context.Context, row dbsqlc.WorkspaceDomain) error {
	now := w.now()
	locked, err := w.locks.Acquire(ctx, row.ID, w.wc.OwnerID, 1, now, now.Add(w.wc.LockTTL))
	if err != nil {
		return err
	}
	if !locked {
		return nil // another server instance is issuing it
	}
	defer func() { _ = w.locks.ReleaseGeneration(context.Background(), row.ID, w.wc.OwnerID, 1) }()
	if w.wc.CAA != nil {
		caa, err := w.wc.CAA.Check(ctx, row.Hostname, w.wc.IssuerName)
		if err != nil {
			return fmt.Errorf("%w: %w", tunnelcert.ErrCAAUnavailable, err)
		}
		if caa.State != "ready" && caa.State != "not_applicable" {
			return tunnelcert.ErrCAABlocked
		}
	}
	wildcard := "*." + row.Hostname
	domain := tunnelcert.Domain{ID: row.ID, AccountID: row.AccountID, TargetKind: tunnelcert.TargetWorkspaceDomain, Hostname: wildcard,
		ChallengeReference: row.ChallengeReference, Generation: 1, Strategy: tunnelcert.StrategyDelegatedDNS01, OwnershipState: "verified", Issuer: w.wc.IssuerName}
	bundle, err := w.wc.Issuer.IssueDNS01Names(ctx, domain, []string{wildcard, row.Hostname})
	if err != nil {
		return err
	}
	defer clear(bundle.PrivateKeyPEM)
	bundle.Issuer = w.wc.IssuerName
	identity, err := bundle.Validate(wildcard, w.now(), w.wc.MaxLifetime)
	if err != nil {
		return err
	}
	if _, err := bundle.Validate(row.Hostname, w.now(), w.wc.MaxLifetime); err != nil {
		return err
	}
	certificate, key, err := tunnelcert.SealParts(ctx, w.wc.Keys, w.wc.MasterKeyReference, bundle)
	if err != nil {
		return err
	}
	_, err = w.db.Queries().StoreWorkspaceDomainCertificate(ctx, dbsqlc.StoreWorkspaceDomainCertificateParams{ID: row.ID,
		MasterKeyReference: sql.NullString{String: w.wc.MasterKeyReference, Valid: true}, CertificateCiphertext: certificate, PrivateKeyCiphertext: key,
		CertificateFingerprint: identity.Fingerprint[:], CertificateExpiresAt: sql.NullTime{Time: identity.NotAfter, Valid: true}, Now: w.now()})
	return err
}

// WorkspaceCertificate opens a domain's current certificate for a hostname under it.
func (w *Worker) WorkspaceCertificate(ctx context.Context, accountID, id string) (tunnelcert.CertificateBundle, error) {
	var row dbsqlc.WorkspaceDomain
	err := w.db.Pool().QueryRow(ctx, `SELECT master_key_reference,certificate_ciphertext,private_key_ciphertext,certificate_expires_at,ownership_state
 FROM paperboat.workspace_domains WHERE id=$1`, id).Scan(&row.MasterKeyReference, &row.CertificateCiphertext, &row.PrivateKeyCiphertext, &row.CertificateExpiresAt, &row.OwnershipState)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (!row.MasterKeyReference.Valid || row.OwnershipState != "verified") {
		return tunnelcert.CertificateBundle{}, tunnelcert.ErrWorkspaceCertificatePending
	}
	if err != nil {
		return tunnelcert.CertificateBundle{}, err
	}
	bundle, err := tunnelcert.OpenParts(ctx, w.wc.Keys, row.MasterKeyReference.String, row.CertificateCiphertext, row.PrivateKeyCiphertext)
	if err != nil {
		return tunnelcert.CertificateBundle{}, err
	}
	bundle.Issuer, bundle.NotAfter = w.wc.IssuerName, row.CertificateExpiresAt.Time
	return bundle, nil
}

func certificateFailureCode(err error) string {
	switch {
	case errors.Is(err, tunnelcert.ErrIssuerRateLimited):
		return "issuer_rate_limited"
	case errors.Is(err, tunnelcert.ErrCAABlocked):
		return "caa_blocked"
	case errors.Is(err, tunnelcert.ErrCAAUnavailable):
		return "caa_unavailable"
	case errors.Is(err, tunnelcert.ErrDNSChallengePending):
		return "dns_challenge_pending"
	case errors.Is(err, tunnelcert.ErrDNSChallengeUnavailable):
		return "dns_challenge_unavailable"
	case errors.Is(err, tunnelcert.ErrCertificateInvalid):
		return "certificate_invalid"
	default:
		return "issuer_unavailable"
	}
}

func (w *Worker) Worker(interval time.Duration, limit int) workers.Worker {
	return func(ctx context.Context) error {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			finish := workers.BeginIteration(ctx, interval)
			err := w.Run(ctx, limit)
			if err != nil && !errors.Is(err, context.Canceled) {
				workers.IterationFailure(ctx, "workspace_domains", "worker_iteration_failed", err)
			}
			finish(nil)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	}
}

// SystemCNAME asks the resolvers in /etc/resolv.conf for name's CNAME record, without following it.
func SystemCNAME(ctx context.Context, name string) (string, error) {
	config, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil || len(config.Servers) == 0 {
		return "", errors.New("DNS resolver configuration is unavailable")
	}
	message := new(dns.Msg)
	message.SetQuestion(dns.Fqdn(name), dns.TypeCNAME)
	client := &dns.Client{Timeout: 5 * time.Second}
	var lastErr error
	for _, server := range config.Servers {
		reply, _, err := client.ExchangeContext(ctx, message, net.JoinHostPort(server, config.Port))
		if err != nil {
			lastErr = err
			continue
		}
		if reply.Rcode != dns.RcodeSuccess && reply.Rcode != dns.RcodeNameError {
			lastErr = fmt.Errorf("DNS answered %s", dns.RcodeToString[reply.Rcode])
			continue
		}
		for _, answer := range reply.Answer {
			if cname, ok := answer.(*dns.CNAME); ok && strings.EqualFold(strings.TrimSuffix(cname.Hdr.Name, "."), name) {
				return strings.ToLower(strings.TrimSuffix(cname.Target, ".")), nil
			}
		}
		return "", nil
	}
	return "", lastErr
}

var _ tunnelcert.WorkspaceCertificateSource = (*Worker)(nil)
