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
	if !tunnel {
		return nil, errors.New("browser HTTPS configuration requires the tunnel component")
	}
	if certPath == "" || keyPath == "" {
		return nil, errors.New("configure-tls requires --https-cert and --https-key")
	}
	if err := validatePublicHTTPS(s.Config.EndpointHost, certPath, keyPath, time.Now()); err != nil {
		return nil, err
	}
	previousCert, previousKey := s.Config.PublicHTTPSCertFile, s.Config.PublicHTTPSKeyFile
	s.Config.PublicHTTPSCertFile, s.Config.PublicHTTPSKeyFile = certPath, keyPath
	if err = save(dir, s); err != nil {
		s.Config.PublicHTTPSCertFile, s.Config.PublicHTTPSKeyFile = previousCert, previousKey
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
		s.Config.PublicHTTPSCertFile, s.Config.PublicHTTPSKeyFile = previousCert, previousKey
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
	if err := validatePublicHTTPS(s.Config.EndpointHost, s.Config.PublicHTTPSCertFile, s.Config.PublicHTTPSKeyFile, time.Now()); err != nil {
		return err
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
		deployment["public_https_cert_file"], _ = json.Marshal(s.Config.PublicHTTPSCertFile)
		deployment["public_https_key_file"], _ = json.Marshal(s.Config.PublicHTTPSKeyFile)
		if err = writeJSON(path, deployment); err != nil {
			return err
		}
	}
	return nil
}
