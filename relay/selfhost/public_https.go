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

func validatePublicHTTPS(host, certPath, keyPath string, now time.Time) error {
	if certPath == "" && keyPath == "" {
		return nil
	}
	if certPath == "" || keyPath == "" {
		return errors.New("browser HTTPS requires both --https-cert and --https-key")
	}
	for _, path := range []string{certPath, keyPath} {
		if !filepath.IsAbs(path) || len(path) > 4096 {
			return errors.New("browser HTTPS certificate paths must be bounded and absolute")
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("cannot read browser HTTPS certificate files: %w", err)
		}
		if !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return errors.New("browser HTTPS certificate files must be bounded regular files")
		}
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("browser HTTPS private key must have owner-only permissions (0600)")
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
		return errors.New("browser HTTPS certificate does not cover the enrolled endpoint hostname")
	}
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return errors.New("browser HTTPS certificate is not currently valid")
	}
	if len(leaf.ExtKeyUsage) > 0 {
		allowed := false
		for _, usage := range leaf.ExtKeyUsage {
			allowed = allowed || usage == x509.ExtKeyUsageServerAuth || usage == x509.ExtKeyUsageAny
		}
		if !allowed {
			return errors.New("browser HTTPS certificate does not permit server authentication")
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
