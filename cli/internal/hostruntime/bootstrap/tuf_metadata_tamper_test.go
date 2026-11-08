package bootstrap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// F14-05 (the metadata half of "tampered metadata or file rejected"): the update client refuses signed metadata whose bytes
// were altered, signed with the wrong key, or older than what it already trusts.

// Every metadata document is checked on its own: altering one signature byte of any of them refuses the update.
func TestFetchVerifiedArtifactRejectsTamperedMetadata(t *testing.T) {
	probe := newTestTUFRepository(t, []byte("pb"), "2026.08.07.1", time.Now().UTC().Add(time.Hour))
	var documents []string
	for path := range probe.files {
		if strings.Contains(path, "/metadata/") && strings.HasSuffix(path, ".json") {
			documents = append(documents, path)
		}
	}
	if len(documents) < 3 {
		t.Fatalf("expected timestamp, snapshot and targets metadata, found %v", documents)
	}
	for _, document := range documents {
		t.Run(filepath.Base(document), func(t *testing.T) {
			repository := newTestTUFRepository(t, []byte("pb"), "2026.08.07.1", time.Now().UTC().Add(time.Hour))
			body := repository.files[document]
			marker := []byte(`"sig":"`)
			at := bytes.Index(body, marker)
			if at < 0 {
				t.Skipf("%s carries no signature", document)
			}
			altered := append([]byte(nil), body...)
			position := at + len(marker)
			if altered[position] == '0' {
				altered[position] = '1'
			} else {
				altered[position] = '0'
			}
			repository.files[document] = altered
			server := repository.server(t)
			defer server.Close()
			if _, err := fetchVerifiedArtifact(context.Background(), descriptor(server.URL, "2026.08.07.1"), filepath.Join(t.TempDir(), "tuf"), server.Client(), repository.root, runtime.GOOS, runtime.GOARCH); err == nil {
				t.Fatalf("metadata %s with an altered signature was accepted", document)
			}
		})
	}
}

// Metadata signed by keys the pinned root does not trust is refused.
func TestFetchVerifiedArtifactRejectsMetadataSignedByAnotherRoot(t *testing.T) {
	trusted := newTestTUFRepository(t, []byte("pb"), "2026.08.07.1", time.Now().UTC().Add(time.Hour))
	attacker := newTestTUFRepository(t, []byte("pb"), "2026.08.07.1", time.Now().UTC().Add(time.Hour))
	server := attacker.server(t)
	defer server.Close()
	if _, err := fetchVerifiedArtifact(context.Background(), descriptor(server.URL, "2026.08.07.1"), filepath.Join(t.TempDir(), "tuf"), server.Client(), trusted.root, runtime.GOOS, runtime.GOARCH); err == nil {
		t.Fatal("metadata signed by keys outside the pinned root was accepted")
	}
}

// Metadata older than what the client already trusts (a rollback to a validly signed earlier snapshot) is refused.
func TestFetchVerifiedArtifactRejectsRolledBackMetadata(t *testing.T) {
	keys := map[string]ed25519.PrivateKey{}
	state := filepath.Join(t.TempDir(), "tuf")
	newer := newTestTUFRepositoryForMetadataVersion(t, []byte("pb"), "2026.08.07.1", time.Now().UTC().Add(time.Hour), runtime.GOOS, runtime.GOARCH, keys, 3)
	server := newer.server(t)
	if _, err := fetchVerifiedArtifact(context.Background(), descriptor(server.URL, "2026.08.07.1"), state, server.Client(), newer.root, runtime.GOOS, runtime.GOARCH); err != nil {
		server.Close()
		t.Fatalf("the current metadata was refused: %v", err)
	}
	server.Close()
	older := newTestTUFRepositoryForMetadataVersion(t, []byte("pb"), "2026.08.07.1", time.Now().UTC().Add(time.Hour), runtime.GOOS, runtime.GOARCH, keys, 1)
	server = older.server(t)
	defer server.Close()
	if _, err := fetchVerifiedArtifact(context.Background(), descriptor(server.URL, "2026.08.07.1"), state, server.Client(), newer.root, runtime.GOOS, runtime.GOARCH); err == nil {
		t.Fatal("a rollback to older signed metadata was accepted")
	}
}
