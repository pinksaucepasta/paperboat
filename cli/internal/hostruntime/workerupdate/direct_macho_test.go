//go:build darwin || linux

package workerupdate

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirectMachOStagingPreservesSignedIdentityAndRejectsUnverifiedBytes(t *testing.T) {
	for _, scenario := range []string{"valid", "digest", "format", "signature"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newFixture(t)
			body := make([]byte, 64)
			binary.LittleEndian.PutUint32(body, 0xfeedfacf)
			binary.LittleEndian.PutUint32(body[4:], 0x0100000c)
			if scenario == "format" {
				body[0] = 0
			}
			candidate := release("2026.10.15.1", body)
			candidate.Platform, candidate.Architecture = "darwin", "arm64"
			if scenario == "digest" {
				candidate.SHA256 = strings.Repeat("f", 64)
			}
			fixture.fetcher.body = body
			fixture.manager.config.NativeVerifier = nativeVerifierFunc(func(_ context.Context, path, platform, arch string) error {
				if platform != "darwin" || arch != "arm64" || !regularMatches(path, candidate.Length, candidate.SHA256) {
					t.Fatal("native validator did not receive the exact authenticated product")
				}
				if scenario == "signature" {
					return errors.New("fixture native signature is invalid")
				}
				return nil
			})
			got, err := fixture.manager.stage(context.Background(), candidate)
			if scenario == "valid" {
				if err != nil || got.SHA256 != candidate.SHA256 || got.Length != candidate.Length {
					t.Fatalf("direct product identity changed: %+v, %v", got, err)
				}
				staged, err := os.ReadFile(fixture.paths.staged)
				if err != nil || !bytes.Equal(staged, body) {
					t.Fatal("staging did not preserve exact signed executable bytes")
				}
				info, err := os.Stat(fixture.paths.staged)
				if err != nil || info.Mode().Perm()&0o111 != 0o111 {
					t.Fatal("verified executable is not runnable by the enrolled worker")
				}
			} else {
				if !errors.Is(err, ErrInvalidRelease) {
					t.Fatalf("unverified product accepted: %v", err)
				}
				if _, err := os.Stat(fixture.paths.staged); !os.IsNotExist(err) {
					t.Fatal("failed verification published a staged executable")
				}
			}
			pending, _ := filepath.Glob(filepath.Join(filepath.Dir(fixture.paths.staged), ".paperboat-runtime-*"))
			if len(pending) != 0 {
				t.Fatal("staging retained uncommitted download bytes")
			}
		})
	}
}
