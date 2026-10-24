package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/relay/selfhost"
)

// The service runs with ProtectHome=true, so operator files under /home cannot
// be read by pbh serve, and the edge container runs as root without
// CAP_DAC_OVERRIDE, so the source owner's mode cannot be relied on either.
// Install therefore copies every operator trust file into the protected state
// directory, owned by root and owner-only, and configures the copies.
type trustFile struct {
	kind        string // file name prefix in the state directory
	label       string // operator-facing name used in errors
	private     bool   // source must be owner-only (private key)
	certificate bool   // source must contain a PEM certificate
}

var (
	publicCertFile = trustFile{kind: "public-https-cert", label: "browser HTTPS certificate", certificate: true}
	publicKeyFile  = trustFile{kind: "public-https-key", label: "browser HTTPS private key", private: true}
	controlCAFile  = trustFile{kind: "control-ca", label: "control-plane CA certificate", certificate: true}
)

const maxTrustFileBytes = 1 << 20

// readTrustFile returns the bounded contents of an operator file. Every error
// names the file so the operator can fix that path and rerun.
func readTrustFile(file trustFile, src string) ([]byte, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", commandError(fmt.Sprintf("cannot read %s %q; check the path exists and is readable by root", file.label, src)), err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", commandError(fmt.Sprintf("cannot inspect %s %q", file.label, src)), err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxTrustFileBytes {
		return nil, commandError(fmt.Sprintf("%s %q must be a regular file of at most 1 MiB", file.label, src))
	}
	if file.private && info.Mode().Perm()&0077 != 0 {
		return nil, commandError(fmt.Sprintf("%s %q must be owner-only (mode 0600)", file.label, src))
	}
	data, err := io.ReadAll(io.LimitReader(f, maxTrustFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", commandError(fmt.Sprintf("cannot read %s %q", file.label, src)), err)
	}
	if len(data) > maxTrustFileBytes {
		return nil, commandError(fmt.Sprintf("%s %q must be a regular file of at most 1 MiB", file.label, src))
	}
	if file.certificate && !x509.NewCertPool().AppendCertsFromPEM(data) {
		return nil, commandError(fmt.Sprintf("%s %q does not contain a PEM certificate", file.label, src))
	}
	return data, nil
}

// stagedTrustPath names the state-directory copy by content. A repeated install
// reuses the same copy, and a rollback keeps the previous copy in place.
func stagedTrustPath(dir string, file trustFile, data []byte) string {
	sum := sha256.Sum256(data)
	return filepath.Join(dir, fmt.Sprintf("%s-%x.pem", file.kind, sum[:8]))
}

// stageTrustFile copies src into the existing state directory dir and returns
// the copy's path.
func stageTrustFile(dir string, file trustFile, src string) (string, error) {
	data, err := readTrustFile(file, src)
	if err != nil {
		return "", err
	}
	path := stagedTrustPath(dir, file, data)
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, data) {
		return path, nil
	}
	// os.CreateTemp makes the file owner-only; rename publishes it whole.
	tmp, err := os.CreateTemp(dir, "."+file.kind+"-*.tmp")
	if err != nil {
		return "", fmt.Errorf("%w: %w", commandError(fmt.Sprintf("cannot store %s in state directory %q; check that it is writable", file.label, dir)), err)
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return "", fmt.Errorf("%w: %w", commandError(fmt.Sprintf("cannot store %s as %q; state retained for retry", file.label, path)), err)
	}
	return path, nil
}

// matchStoredTrustFile accepts a repeated flag only when it names the content
// already staged for this installation.
func matchStoredTrustFile(dir string, file trustFile, flagName, src, stored string) error {
	data, err := readTrustFile(file, src)
	if err != nil {
		return err
	}
	if stored == "" || stagedTrustPath(dir, file, data) != stored {
		return commandError(fmt.Sprintf("existing --%s differs; omit that flag to preserve installation settings", flagName))
	}
	return nil
}

// stageInstallFiles copies the trust files given to a fresh install into the
// state directory and points the configuration at the copies.
func stageInstallFiles(dir string, fs *flag.FlagSet, c selfhost.Config) (selfhost.Config, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return c, err
	}
	var err error
	if src := fs.Lookup("https-cert").Value.String(); src != "" {
		if c.PublicHTTPSCertFile, err = stageTrustFile(dir, publicCertFile, src); err != nil {
			return c, err
		}
	}
	if src := fs.Lookup("https-key").Value.String(); src != "" {
		if c.PublicHTTPSKeyFile, err = stageTrustFile(dir, publicKeyFile, src); err != nil {
			return c, err
		}
	}
	if src := fs.Lookup("control-ca").Value.String(); src != "" {
		if c.ControlCA, err = stageTrustFile(dir, controlCAFile, src); err != nil {
			return c, err
		}
	}
	return c, nil
}
