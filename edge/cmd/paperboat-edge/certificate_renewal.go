package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/pinksaucepasta/paperboat/edge/internal/control"
)

// maxCertificateRenewalStatus bounds the status file the host's renewal timer writes (a bool, a 300-character reason
// and a timestamp).
const maxCertificateRenewalStatus = 4096

// certificateRenewal reads the host renewal timer's last check for each heartbeat. A missing, oversized or malformed
// file sends nothing: the server then shows no certificate entry for this node, and a failed check never stops the beat.
func certificateRenewal(path string) func() *control.CertificateRenewal {
	if path == "" {
		return nil
	}
	return func() *control.CertificateRenewal {
		status, err := readCertificateRenewal(path)
		if err != nil {
			if !os.IsNotExist(err) {
				slog.Warn("certificate renewal status unreadable", "path", path, "error", err)
			}
			return nil
		}
		return status
	}
}

func readCertificateRenewal(path string) (*control.CertificateRenewal, error) {
	// A FIFO or device would block or never end: only a regular file (not a link) is read.
	if info, err := os.Lstat(path); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, errInvalidRenewalStatus
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errInvalidRenewalStatus
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxCertificateRenewalStatus+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxCertificateRenewalStatus {
		return nil, errInvalidRenewalStatus
	}
	var status struct {
		OK        *bool     `json:"ok"`
		Error     string    `json:"error"`
		CheckedAt time.Time `json:"checked_at"`
	}
	if json.Unmarshal(raw, &status) != nil || status.OK == nil || status.CheckedAt.IsZero() || len(status.Error) > 1024 || !utf8.ValidString(status.Error) {
		return nil, errInvalidRenewalStatus
	}
	return &control.CertificateRenewal{OK: *status.OK, Error: status.Error, CheckedAt: status.CheckedAt.UTC()}, nil
}

type renewalStatusError string

func (e renewalStatusError) Error() string { return string(e) }

const errInvalidRenewalStatus = renewalStatusError("invalid certificate renewal status file")
