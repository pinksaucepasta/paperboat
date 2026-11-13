package tlscert

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
)

// An install handover keeps the edge's process epoch, and the server sends each certificate action
// once per (node, epoch) and wipes the key material when it is acknowledged. The replacement process
// therefore cannot receive its certificates again: the serving process hands them over instead
// (edge/internal/handover), and the replacement stages them through the same checks as a server
// delivery. Prior art: nginx binary upgrade and Envoy hot restart pass listening sockets, not TLS
// state; a certificate cache that the replacement cannot rebuild must be passed explicitly.

// ExportedCertificate is one active certificate carried across a handover.
type ExportedCertificate struct {
	Binding Binding
	Bundle  Bundle
}

// ImportReport counts an import's outcome by reason only; it never names a hostname or key.
type ImportReport struct {
	Imported int
	Dropped  map[string]int
}

// ExportEncoded returns the active, unexpired certificates as opaque JSON records for the handover
// socket. The records hold private keys: the caller sends them and wipes them (WipeEncoded).
func (r *Registry) ExportEncoded() ([][]byte, error) {
	now := r.now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrClosed
	}
	var records [][]byte
	for _, e := range r.entries {
		if e.state != StateActive || !e.notAfter.After(now) {
			continue
		}
		record, err := encodeEntry(e)
		if err != nil {
			WipeEncoded(records)
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

// encodeEntry is the opaque record of one certificate: its binding and bundle, with the private key. The caller
// wipes it.
func encodeEntry(e *entry) ([]byte, error) {
	var chain []byte
	for _, der := range e.certificate.Certificate {
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(e.certificate.PrivateKey)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	clear(keyDER)
	record, err := json.Marshal(ExportedCertificate{Binding: e.binding, Bundle: Bundle{CertificatePEM: chain, PrivateKeyPEM: keyPEM, Issuer: e.issuer, NotBefore: e.notBefore, NotAfter: e.notAfter}})
	clear(keyPEM)
	clear(chain)
	return record, err
}

// ImportEncoded stages, readies and activates handed-over certificates through the registry's own
// checks (chain and key match, hostname, lifetime, generation fences) and only for this node and
// epoch. A record that fails any check is dropped and counted; the import never fails the start.
// The records are wiped before it returns.
func (r *Registry) ImportEncoded(ctx context.Context, nodeID, epoch string, records [][]byte) ImportReport {
	defer WipeEncoded(records)
	report := ImportReport{Dropped: map[string]int{}}
	for _, record := range records {
		var item ExportedCertificate
		if json.Unmarshal(record, &item) != nil {
			report.Dropped["malformed"]++
			continue
		}
		reason := r.importOne(ctx, nodeID, epoch, item)
		clear(item.Bundle.PrivateKeyPEM)
		clear(item.Bundle.CertificatePEM)
		if reason != "" {
			report.Dropped[reason]++
			continue
		}
		report.Imported++
	}
	return report
}

func (r *Registry) importOne(ctx context.Context, nodeID, epoch string, item ExportedCertificate) string {
	if item.Binding.EdgeNodeID != nodeID || item.Binding.EdgeProcessEpoch != epoch {
		return "other_process"
	}
	for _, step := range []func() error{
		func() error { return r.stage(ctx, item.Binding, item.Bundle, nil) },
		func() error { return r.markReady(ctx, item.Binding, nil) },
		func() error { return r.activate(ctx, item.Binding, nil) },
	} {
		if err := step(); err != nil {
			return importDropReason(err)
		}
	}
	return ""
}

func importDropReason(err error) string {
	var invalid x509.CertificateInvalidError
	switch {
	case errors.Is(err, ErrCertificateExpired) || errors.As(err, &invalid) && invalid.Reason == x509.Expired:
		return "expired"
	case errors.Is(err, ErrGenerationConflict) || errors.Is(err, ErrHostnameConflict):
		return "conflict"
	case errors.Is(err, ErrCertificateInvalid):
		return "invalid"
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	}
	return "other"
}

// WipeEncoded zeroes encoded certificate records.
func WipeEncoded(records [][]byte) {
	for _, record := range records {
		clear(record)
	}
}
