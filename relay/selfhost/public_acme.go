package selfhost

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"golang.org/x/crypto/acme"
)

// Automatic browser HTTPS for a managed node: the node obtains its endpoint host's certificate from an ACME CA
// (HTTP-01, answered by the edge from the challenge directory below) and renews it once two thirds of its lifetime
// have passed, the rule certmagic and the server's own certificates use (RenewalAt = NotAfter - RenewBefore). A failed
// attempt is recorded for `pbh status` and `pbh doctor` and retried at the next check; the certificate being replaced
// serves meanwhile. Replaces the manual `pbh configure-tls --https-cert/--https-key` renewals (and the external
// certbot timer) for nodes that opt in with `pbh configure-tls --acme`.
const (
	publicACMEDirName = "public-acme"
	// Let's Encrypt issues certificates for an IP address only under its short-lived profile; it is used when the CA
	// advertises it (the CA's directory lists its profiles).
	publicACMEProfile = "shortlived"
	// PublicACMEDirectoryURL is the CA directory the node requests from.
	PublicACMEDirectoryURL = acme.LetsEncryptURL
	publicACMERenewAfter   = 2.0 / 3
	publicACMEAttemptLimit = 3 * time.Minute
)

var acmeTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

// PublicACMEChallengeDir is where HTTP-01 answers are written and where the edge serves them from.
func PublicACMEChallengeDir(dir string) string {
	return filepath.Join(dir, publicACMEDirName, "challenge")
}

func publicACMEFile(dir, name string) string { return filepath.Join(dir, publicACMEDirName, name) }

// PublicHTTPSStatus is what the last renewal check left: for `pbh status`, `pbh doctor` and the operator.
type PublicHTTPSStatus struct {
	Automatic bool      `json:"automatic"`
	NotAfter  time.Time `json:"not_after,omitzero"`
	RenewAt   time.Time `json:"renew_at,omitzero"`
	// FailingSince and LastError are set while the last attempt failed; LastError is the CA's or the client's reason.
	FailingSince time.Time `json:"failing_since,omitzero"`
	LastError    string    `json:"last_error,omitempty"`
}

// publicHTTPSRenewal reports whether the certificate at certPath is due: two thirds of its validity have passed (or it is
// missing, unreadable or expired), and when it ends and should be renewed.
func publicHTTPSRenewal(certPath string, now time.Time) (due bool, notAfter, renewAt time.Time) {
	pair, err := os.ReadFile(certPath)
	if err != nil {
		return true, time.Time{}, time.Time{}
	}
	block, _ := pem.Decode(pair)
	if block == nil {
		return true, time.Time{}, time.Time{}
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return true, time.Time{}, time.Time{}
	}
	renewAt = leaf.NotBefore.Add(time.Duration(float64(leaf.NotAfter.Sub(leaf.NotBefore)) * publicACMERenewAfter))
	return !now.Before(renewAt) || !now.Before(leaf.NotAfter), leaf.NotAfter, renewAt
}

// ReadPublicHTTPSStatus is the recorded state of automatic browser HTTPS; Automatic is false when it is not enabled.
func ReadPublicHTTPSStatus(dir string) (PublicHTTPSStatus, error) {
	s, err := load(dir)
	if err != nil || !s.Config.PublicHTTPSAutomatic {
		return PublicHTTPSStatus{}, err
	}
	status := PublicHTTPSStatus{Automatic: true}
	if raw, err := os.ReadFile(publicACMEFile(dir, "status.json")); err == nil {
		_ = json.Unmarshal(raw, &status)
		status.Automatic = true
	}
	return status, nil
}

// RenewPublicHTTPS obtains or renews the endpoint host's certificate when automatic browser HTTPS is on and the
// certificate is due. It reports whether a new certificate was installed; the caller restarts the runtime on it.
// directoryURL is the ACME directory (PublicACMEDirectoryURL; tests pass their own CA).
func RenewPublicHTTPS(ctx context.Context, dir, directoryURL string, now time.Time) (bool, error) {
	lock, err := lockState(dir)
	if err != nil {
		return false, err
	}
	s, err := load(dir)
	lock.Close() // the lock is not held over the network: other pbh commands must not wait on a CA
	if err != nil {
		return false, err
	}
	if !s.Config.PublicHTTPSAutomatic {
		return false, nil
	}
	certPath, keyPath := publicACMEFile(dir, "cert.pem"), publicACMEFile(dir, "key.pem")
	due, notAfter, renewAt := publicHTTPSRenewal(certPath, now)
	if !due {
		return false, recordPublicHTTPSStatus(dir, PublicHTTPSStatus{NotAfter: notAfter, RenewAt: renewAt}, now)
	}
	ctx, cancel := context.WithTimeout(ctx, publicACMEAttemptLimit)
	defer cancel()
	issueErr := obtainPublicHTTPS(ctx, dir, s.Config.EndpointHost, directoryURL, certPath, keyPath)
	if issueErr == nil {
		issueErr = installPublicHTTPS(dir, certPath, keyPath, now)
	}
	if issueErr != nil {
		return false, errors.Join(issueErr, recordPublicHTTPSStatus(dir, PublicHTTPSStatus{NotAfter: notAfter, RenewAt: renewAt, LastError: issueErr.Error()}, now))
	}
	_, notAfter, renewAt = publicHTTPSRenewal(certPath, now)
	return true, recordPublicHTTPSStatus(dir, PublicHTTPSStatus{NotAfter: notAfter, RenewAt: renewAt}, now)
}

// recordPublicHTTPSStatus writes the outcome; FailingSince keeps the first failure of a run of them.
func recordPublicHTTPSStatus(dir string, status PublicHTTPSStatus, now time.Time) error {
	path := publicACMEFile(dir, "status.json")
	if status.LastError != "" {
		status.LastError = status.LastError[:min(len(status.LastError), 300)]
		status.FailingSince = now
		var previous PublicHTTPSStatus
		if raw, err := os.ReadFile(path); err == nil && json.Unmarshal(raw, &previous) == nil && previous.LastError != "" && !previous.FailingSince.IsZero() {
			status.FailingSince = previous.FailingSince
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return writeJSON(path, status)
}

// installPublicHTTPS points the installation at the certificate it just obtained (the first time) and validates it
// the way every start does.
func installPublicHTTPS(dir, certPath, keyPath string, now time.Time) error {
	lock, err := lockState(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	s, err := load(dir)
	if err != nil {
		return err
	}
	if err := validatePublicHTTPS(s.Config.EndpointHost, certPath, keyPath, now); err != nil {
		return err
	}
	if s.Config.PublicHTTPSCertFile == certPath && s.Config.PublicHTTPSKeyFile == keyPath {
		return nil
	}
	s.Config.PublicHTTPSCertFile, s.Config.PublicHTTPSKeyFile = certPath, keyPath
	return save(dir, s)
}

// obtainPublicHTTPS runs one ACME order for host and writes the certificate chain and its key (0600, atomically).
func obtainPublicHTTPS(ctx context.Context, dir, host, directoryURL, certPath, keyPath string) error {
	if err := os.MkdirAll(PublicACMEChallengeDir(dir), 0700); err != nil {
		return err
	}
	accountKey, err := loadOrCreateACMEAccountKey(publicACMEFile(dir, "account.key"))
	if err != nil {
		return err
	}
	client := &acme.Client{Key: accountKey, DirectoryURL: directoryURL, UserAgent: "paperboat-pbh"}
	if _, err := client.Register(ctx, &acme.Account{}, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return fmt.Errorf("register the ACME account: %w", err)
	}
	identifier := acme.AuthzID{Type: "dns", Value: host}
	if net.ParseIP(host) != nil {
		identifier.Type = "ip"
	}
	var options []acme.OrderOption
	if directory, err := client.Discover(ctx); err == nil {
		if _, ok := directory.Profiles[publicACMEProfile]; ok {
			options = append(options, acme.WithOrderIssuanceProfile(publicACMEProfile))
		}
	}
	order, err := client.AuthorizeOrder(ctx, []acme.AuthzID{identifier}, options...)
	if err != nil {
		return fmt.Errorf("start the ACME order: %w", err)
	}
	for _, authzURL := range order.AuthzURLs {
		if err := answerHTTP01(ctx, client, PublicACMEChallengeDir(dir), authzURL); err != nil {
			return err
		}
	}
	if order, err = client.WaitOrder(ctx, order.URI); err != nil {
		return fmt.Errorf("wait for the ACME order: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	request := &x509.CertificateRequest{}
	if ip := net.ParseIP(host); ip != nil {
		request.IPAddresses = []net.IP{ip}
	} else {
		request.DNSNames = []string{host}
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, request, key)
	if err != nil {
		return err
	}
	chain, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return fmt.Errorf("finalize the ACME order: %w", err)
	}
	var certPEM []byte
	for _, der := range chain {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	// The key first: a certificate without its key would be an unusable pair if the second write failed.
	if err := writeFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})); err != nil {
		return err
	}
	return writeFile(certPath, certPEM)
}

// answerHTTP01 places the HTTP-01 key authorization where the edge serves it, accepts the challenge, and waits for
// the CA to validate it; the file is removed on every exit path.
func answerHTTP01(ctx context.Context, client *acme.Client, challengeDir, authzURL string) error {
	authz, err := client.GetAuthorization(ctx, authzURL)
	if err != nil {
		return fmt.Errorf("read the ACME authorization: %w", err)
	}
	if authz.Status == acme.StatusValid {
		return nil
	}
	var challenge *acme.Challenge
	for _, c := range authz.Challenges {
		if c.Type == "http-01" {
			challenge = c
		}
	}
	if challenge == nil || !acmeTokenPattern.MatchString(challenge.Token) {
		return errors.New("the CA offered no usable HTTP-01 challenge")
	}
	response, err := client.HTTP01ChallengeResponse(challenge.Token)
	if err != nil {
		return err
	}
	path := filepath.Join(challengeDir, challenge.Token)
	if err := writeFile(path, []byte(response)); err != nil {
		return err
	}
	defer os.Remove(path)
	if _, err := client.Accept(ctx, challenge); err != nil {
		return fmt.Errorf("accept the HTTP-01 challenge: %w", err)
	}
	if _, err := client.WaitAuthorization(ctx, authzURL); err != nil {
		return fmt.Errorf("the CA could not validate the HTTP-01 challenge (is TCP port 80 of %s reachable from the internet?): %w", authz.Identifier.Value, err)
	}
	return nil
}

func loadOrCreateACMEAccountKey(path string) (*ecdsa.PrivateKey, error) {
	if raw, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(raw)
		if block == nil {
			return nil, errors.New("the ACME account key file is unreadable; delete it to register a new account")
		}
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("the ACME account key file is unreadable; delete it to register a new account: %w", err)
		}
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return key, writeFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

// Describe is the one line `pbh status` and `pbh doctor` print for automatic browser HTTPS.
func (s PublicHTTPSStatus) Describe() string {
	switch {
	case !s.Automatic:
		return ""
	case s.LastError != "":
		return fmt.Sprintf("browser HTTPS renewal FAILING since %s: %s", s.FailingSince.UTC().Format(time.RFC3339), s.LastError)
	case s.NotAfter.IsZero():
		return "browser HTTPS: automatic, certificate not issued yet"
	}
	return fmt.Sprintf("browser HTTPS: automatic, valid until %s, renews after %s", s.NotAfter.UTC().Format(time.RFC3339), s.RenewAt.UTC().Format(time.RFC3339))
}
