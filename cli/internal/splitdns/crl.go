package splitdns

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

const CRLPathPrefix = "/.paperboat/crl/"
const MaxCRLBytes = 64 << 10
const crlLifetime = 24 * time.Hour

// CRLPath binds the distribution point to this installation's trusted issuer.
func CRLPath(rootDER []byte) string {
	digest := sha256.Sum256(rootDER)
	return CRLPathPrefix + hex.EncodeToString(digest[:]) + ".crl"
}

// ValidateCRL verifies issuer, signature, validity and bounded contents before
// the unprivileged gateway can serve a guard-signed revocation list.
func ValidateCRL(rootDER, der []byte, now time.Time) (*x509.RevocationList, error) {
	if len(der) == 0 || len(der) > MaxCRLBytes {
		return nil, errors.New("invalid local revocation list size")
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		return nil, err
	}
	list, err := x509.ParseRevocationList(der)
	if err != nil {
		return nil, err
	}
	if err = list.CheckSignatureFrom(root); err != nil {
		return nil, err
	}
	if !bytes.Equal(list.AuthorityKeyId, root.SubjectKeyId) || list.Number == nil || list.Number.Sign() <= 0 || list.Number.BitLen() > 128 || list.ThisUpdate.After(now) || !now.Before(list.NextUpdate) || !list.NextUpdate.After(list.ThisUpdate) || list.NextUpdate.After(root.NotAfter) {
		return nil, errors.New("invalid local revocation list issuer or validity")
	}
	return list, nil
}

// RevocationList publishes the CA's real current revocation state. No leaf
// revocations exist in the local-CA policy; root trust removal ends its authority.
// Persisting the signed list preserves monotonically increasing CRL numbers
// across process restarts, while refusing clock regression or corrupt state.
func (c *CA) RevocationList(now time.Time) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	path := filepath.Join(c.directory, "rootCA.crl")
	number := big.NewInt(1)
	file, openErr := os.Open(path)
	var body []byte
	if openErr == nil {
		var readErr error
		body, readErr = io.ReadAll(io.LimitReader(file, MaxCRLBytes+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			return nil, errors.Join(readErr, closeErr)
		}
	}
	if openErr == nil {
		if len(body) > MaxCRLBytes {
			return nil, errors.New("local revocation state exceeds size limit")
		}
		list, err := x509.ParseRevocationList(body)
		if err != nil || list.CheckSignatureFrom(c.caCert) != nil || list.Number == nil || list.Number.Sign() <= 0 || list.Number.BitLen() > 128 || !bytes.Equal(list.AuthorityKeyId, c.caCert.SubjectKeyId) || list.ThisUpdate.After(now) {
			return nil, errors.New("local revocation state is invalid or clock moved backwards")
		}
		if list.NextUpdate.After(now.Add(time.Hour)) {
			if _, err := ValidateCRL(c.caCert.Raw, body, now); err != nil {
				return nil, err
			}
			return body, nil
		}
		number.Add(list.Number, big.NewInt(1))
	} else if !os.IsNotExist(openErr) {
		return nil, openErr
	}
	if number.BitLen() > 128 || !now.Add(time.Hour).Before(c.caCert.NotAfter) {
		return nil, errors.New("local CA cannot publish a current revocation list")
	}
	next := now.Add(crlLifetime).UTC().Truncate(time.Second)
	if next.After(c.caCert.NotAfter) {
		next = c.caCert.NotAfter
	}
	der, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{Number: number, ThisUpdate: now.Add(-time.Minute).UTC().Truncate(time.Second), NextUpdate: next}, c.caCert, c.caKey)
	if err != nil {
		return nil, err
	}
	if err = atomicWrite(path, der, 0644); err != nil {
		return nil, err
	}
	return der, nil
}
