package selfhost

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// OperatorError is a fixed, private-data-free message an operator can act on;
// pbh prints it instead of the generic failure line.
type OperatorError string

func (e OperatorError) Error() string { return string(e) }

// requireManaged refuses operator HTTPS certificates on customer installations: browser terminals are
// served by managed edges only, so only a managed (global-scope) installation has a browser-trusted host.
func requireManaged(s state) error {
	if s.Claim == nil || s.Claim.ScopeType != "global" {
		return OperatorError("browser HTTPS certificates are for Paperboat-managed installations only; a self-hosted node serves no browser on its endpoint host and needs no certificate")
	}
	return nil
}

func validatePublicHTTPS(host, certPath, keyPath string, now time.Time) error {
	if certPath == "" && keyPath == "" {
		return nil
	}
	if certPath == "" || keyPath == "" {
		return OperatorError("browser HTTPS requires both --https-cert and --https-key")
	}
	for _, path := range []string{certPath, keyPath} {
		if !filepath.IsAbs(path) || len(path) > 4096 {
			return OperatorError("browser HTTPS certificate paths must be bounded and absolute")
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("cannot read browser HTTPS certificate files: %w", err)
		}
		if !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return OperatorError("browser HTTPS certificate files must be bounded regular files")
		}
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return OperatorError("browser HTTPS private key must have owner-only permissions (0600)")
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("browser HTTPS certificate/key pair is invalid: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	if err = leaf.VerifyHostname(host); err != nil {
		return OperatorError("browser HTTPS certificate does not cover the enrolled endpoint hostname")
	}
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return OperatorError(fmt.Sprintf("browser HTTPS certificate is valid only from %s to %s; renew it (paperboat-edge-certificate.service needs TCP 80 free) and rerun pbh install", leaf.NotBefore.UTC().Format(time.RFC3339), leaf.NotAfter.UTC().Format(time.RFC3339)))
	}
	if len(leaf.ExtKeyUsage) > 0 {
		allowed := false
		for _, usage := range leaf.ExtKeyUsage {
			allowed = allowed || usage == x509.ExtKeyUsageServerAuth || usage == x509.ExtKeyUsageAny
		}
		if !allowed {
			return OperatorError("browser HTTPS certificate does not permit server authentication")
		}
	}
	return nil
}

// ConfigurePublicHTTPS changes only authoritative public-certificate paths.
// The carrier certificate, enrollment identity, and claim remain untouched.
// Rollback rejects concurrent changes rather than overwriting another operator.
func ConfigurePublicHTTPS(dir, certPath, keyPath string) (rollback func() error, err error) {
	lock, err := lockState(dir)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	s, err := load(dir)
	if err != nil {
		return nil, err
	}
	tunnel := false
	for _, c := range s.Config.Components {
		tunnel = tunnel || c.Capability == "tunnel"
	}
	if err := requireManaged(s); err != nil {
		return nil, err
	}
	if !tunnel {
		return nil, errors.New("browser HTTPS configuration requires the tunnel component")
	}
	if certPath == "" || keyPath == "" {
		return nil, errors.New("configure-tls requires --https-cert and --https-key")
	}
	if err := validatePublicHTTPS(s.Config.EndpointHost, certPath, keyPath, time.Now()); err != nil {
		return nil, err
	}
	previousCert, previousKey, previousAutomatic := s.Config.PublicHTTPSCertFile, s.Config.PublicHTTPSKeyFile, s.Config.PublicHTTPSAutomatic
	// Files given by hand replace automatic issuance: one owner of the certificate.
	s.Config.PublicHTTPSCertFile, s.Config.PublicHTTPSKeyFile, s.Config.PublicHTTPSAutomatic = certPath, keyPath, false
	if err = save(dir, s); err != nil {
		s.Config.PublicHTTPSCertFile, s.Config.PublicHTTPSKeyFile, s.Config.PublicHTTPSAutomatic = previousCert, previousKey, previousAutomatic
		return nil, errors.Join(err, save(dir, s))
	}
	return func() error {
		lock, err := lockState(dir)
		if err != nil {
			return err
		}
		defer lock.Close()
		s, err := load(dir)
		if err != nil {
			return err
		}
		if s.Config.PublicHTTPSCertFile != certPath || s.Config.PublicHTTPSKeyFile != keyPath {
			return errors.New("public HTTPS configuration changed concurrently; restore manually")
		}
		s.Config.PublicHTTPSCertFile, s.Config.PublicHTTPSKeyFile, s.Config.PublicHTTPSAutomatic = previousCert, previousKey, previousAutomatic
		return save(dir, s)
	}, nil
}

// ConfigureAutomaticPublicHTTPS turns automatic browser HTTPS on (public_acme.go): the node then obtains the
// certificate for its endpoint host itself and renews it. The first certificate arrives within a minute of the
// runtime starting; until then the node serves its enrolled identity. A certificate configured by hand is replaced
// by the automatic one once issued.
func ConfigureAutomaticPublicHTTPS(dir string) (rollback func() error, err error) {
	lock, err := lockState(dir)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	s, err := load(dir)
	if err != nil {
		return nil, err
	}
	if err := requireManaged(s); err != nil {
		return nil, err
	}
	tunnel := false
	for _, c := range s.Config.Components {
		tunnel = tunnel || c.Capability == "tunnel"
	}
	if !tunnel {
		return nil, errors.New("browser HTTPS configuration requires the tunnel component")
	}
	previous := s.Config.PublicHTTPSAutomatic
	s.Config.PublicHTTPSAutomatic = true
	if err := save(dir, s); err != nil {
		return nil, err
	}
	return func() error {
		lock, err := lockState(dir)
		if err != nil {
			return err
		}
		defer lock.Close()
		s, err := load(dir)
		if err != nil {
			return err
		}
		s.Config.PublicHTTPSAutomatic = previous
		return save(dir, s)
	}, nil
}

// The state document owns configuration. Derived deployment files are rebuilt
// before starting processes, so interrupted configuration needs no partial-file
// recovery or second authoritative setting.
func syncPublicHTTPSDeployment(dir string) error {
	lock, err := lockState(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	s, err := load(dir)
	if err != nil {
		return err
	}
	if !s.RuntimeReady {
		return nil
	}
	if s.Config.PublicHTTPSCertFile != "" || s.Config.PublicHTTPSKeyFile != "" || s.Config.PublicHTTPSAutomatic {
		if err := requireManaged(s); err != nil {
			return err
		}
	}
	certFile, keyFile := s.Config.PublicHTTPSCertFile, s.Config.PublicHTTPSKeyFile
	if err := validatePublicHTTPS(s.Config.EndpointHost, certFile, keyFile, time.Now()); err != nil {
		if !s.Config.PublicHTTPSAutomatic {
			return err
		}
		// An automatic certificate that has expired (or was damaged) must not keep the node from starting: it
		// serves its enrolled identity until the renewal installs a new one.
		certFile, keyFile = "", ""
	}
	for _, c := range s.Config.Components {
		if c.Capability != "tunnel" {
			continue
		}
		path := filepath.Join(dir, "tunnel", "deployment.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var deployment map[string]json.RawMessage
		if err = json.Unmarshal(raw, &deployment); err != nil {
			return err
		}
		deployment["public_https_cert_file"], _ = json.Marshal(certFile)
		deployment["public_https_key_file"], _ = json.Marshal(keyFile)
		if s.Config.PublicHTTPSAutomatic {
			deployment["public_acme_challenge_dir"], _ = json.Marshal(PublicACMEChallengeDir(dir))
		} else {
			delete(deployment, "public_acme_challenge_dir")
		}
		if err = writeJSON(path, deployment); err != nil {
			return err
		}
	}
	return nil
}
