package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCertificateRenewalStatusFile(t *testing.T) {
	if certificateRenewal("") != nil {
		t.Fatal("no status file configured must send nothing")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "renew-status.json")
	read := certificateRenewal(path)
	if read() != nil {
		t.Fatal("a missing file must send nothing")
	}
	if err := os.WriteFile(path, []byte(`{"ok":false,"error":"certbot failed","checked_at":"2026-11-21T12:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := read()
	if got == nil || got.OK || got.Error != "certbot failed" || !got.CheckedAt.Equal(time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("status = %+v", got)
	}
	for _, bad := range []string{`{`, `{"error":"x","checked_at":"2026-11-21T12:00:00Z"}`, `{"ok":true,"error":""}`, `{"ok":true,"checked_at":"2026-11-21T12:00:00Z","error":"` + strings.Repeat("x", 5000) + `"}`} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if read() != nil {
			t.Fatalf("malformed status %.40q sent", bad)
		}
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if certificateRenewal(link)() != nil {
		t.Fatal("a symlinked status file must not be followed")
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if certificateRenewal(fifo)() != nil {
		t.Fatal("a FIFO must be refused without blocking")
	}
}
