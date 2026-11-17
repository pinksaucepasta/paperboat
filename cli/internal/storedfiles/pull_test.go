package storedfiles

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/filetransfer"
	receiver "github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/store"
	wire "github.com/pinksaucepasta/paperboat/shared/storedfiles"
	"github.com/pinksaucepasta/paperboat/shared/teambucket"
)

func storedFixture(t *testing.T, data []byte) (*Prepared, []wire.File, []byte, string) {
	t.Helper()
	key := bytes.Repeat([]byte{7}, 32)
	hash := sha256.Sum256(data)
	files := []wire.File{{Basename: "proof.bin", Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}}
	manifest := ManifestDigest(files)
	prepared, err := Prepare(t.Context(), "request_1", manifest, []filetransfer.Source{{Basename: "proof.bin", Size: int64(len(data)), SHA256: hash, Reader: bytes.NewReader(data)}}, key, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := prepared.Close(); err != nil {
			t.Error(err)
		}
	})
	return prepared, files, key, manifest
}

func openReceiver(t *testing.T, root string) (*receiver.Service, *store.Store) {
	t.Helper()
	durable, err := store.Open(t.Context(), store.Config{Root: filepath.Join(root, "state")})
	if err != nil {
		t.Fatal(err)
	}
	service, err := receiver.New(receiver.Config{Root: filepath.Join(root, "spool"), LocalMachineID: "machine_recipient", Store: durable, PublishRoot: func() (string, error) { return filepath.Join(root, "Inbox"), nil }})
	if err != nil {
		t.Fatal(err)
	}
	return service, durable
}

func pullConfig(prepared *Prepared, files []wire.File, key []byte, manifest string, service *receiver.Service, base string) PullConfig {
	return PullConfig{RequestID: "request_1", ManifestDigest: manifest, MachineID: "machine_recipient", SourceMachineID: "machine_sender", SenderAccount: "account_sender", Files: files, Metadata: wire.Metadata{Parts: prepared.Parts}, Key: key, Receiver: service, URLs: func(_ context.Context, first, count int) ([]teambucket.Authority, error) {
		urls := make([]teambucket.Authority, count)
		for i := range urls {
			urls[i] = teambucket.Authority{URL: fmt.Sprintf("%s/%d", base, first+i)}
		}
		return urls, nil
	}}
}

func TestStoredPullResumesAfterRestartAndPublishesOnce(t *testing.T) {
	data := bytes.Repeat([]byte("resume-proof-"), (PartPlainBytes+2*(1<<20))/13+1)
	prepared, files, key, manifest := storedFixture(t, data)
	root := t.TempDir()
	service, durable := openReceiver(t, root)
	var mu sync.Mutex
	failSecond := true
	reads := make([]int, len(prepared.Parts))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		index, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil || index < 0 || index >= len(reads) {
			http.Error(w, "invalid", 400)
			return
		}
		reads[index]++
		if index == 1 && failSecond {
			w.WriteHeader(503)
			return
		}
		part, err := prepared.ReadPart(index)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write(part)
	}))
	defer server.Close()
	c := pullConfig(prepared, files, key, manifest, service, server.URL)
	if err := Pull(t.Context(), c); err == nil {
		t.Fatal("interrupted bucket response succeeded")
	}
	transfers, err := service.Batch(t.Context(), "stored:request_1")
	if err != nil || len(transfers) != 1 || transfers[0].CommittedOffset != PartPlainBytes {
		t.Fatalf("resume position: %+v, %v", transfers, err)
	}
	if err := durable.Close(); err != nil {
		t.Fatal(err)
	}
	service, durable = openReceiver(t, root)
	defer durable.Close()
	mu.Lock()
	failSecond = false
	mu.Unlock()
	c.Receiver = service
	if err := Pull(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if err := Pull(t.Context(), c); err != nil {
		t.Fatalf("repeat after publication: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if reads[0] != 1 || reads[1] != 2 {
		t.Fatalf("downloaded already committed parts: %v", reads)
	}
	delivered, err := os.ReadFile(filepath.Join(root, "Inbox", "proof.bin"))
	if err != nil || !bytes.Equal(delivered, data) {
		t.Fatalf("Inbox bytes differ: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "Inbox"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("duplicate Inbox publication: %d, %v", len(entries), err)
	}
}

func TestStoredPullRejectsTamperedCiphertextBeforeCommit(t *testing.T) {
	prepared, files, key, manifest := storedFixture(t, []byte("only authenticated bytes reach the Inbox"))
	data, err := prepared.ReadPart(0)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	// Even a server replacing the ciphertext digest cannot forge the AEAD tag.
	metadata := wire.Metadata{Parts: append([]wire.Part(nil), prepared.Parts...)}
	metadata.Parts[0].SHA256 = digest(data)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) }))
	defer server.Close()
	root := t.TempDir()
	service, durable := openReceiver(t, root)
	defer durable.Close()
	c := pullConfig(prepared, files, key, manifest, service, server.URL)
	c.Metadata = metadata
	if err := Pull(t.Context(), c); !errors.Is(err, ErrInvalid) {
		t.Fatalf("tampered ciphertext accepted: %v", err)
	}
	transfers, err := service.Batch(t.Context(), "stored:request_1")
	if err != nil || transfers[0].CommittedOffset != 0 || transfers[0].State != "created" {
		t.Fatalf("unauthenticated bytes committed: %+v, %v", transfers, err)
	}
	if _, err := os.Stat(filepath.Join(root, "Inbox", "proof.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unauthenticated file published: %v", err)
	}
	// The sealed manifest digest also protects names from a server-side substitution.
	c.Files = append([]wire.File(nil), files...)
	c.Files[0].Basename = "changed.bin"
	if err := Pull(t.Context(), c); !errors.Is(err, ErrInvalid) {
		t.Fatalf("manifest substitution accepted: %v", err)
	}
}

func TestStoredPrepareChecksSourceAndCleansCiphertext(t *testing.T) {
	directory := t.TempDir()
	old := sha256.Sum256([]byte("measured bytes"))
	files := []wire.File{{Basename: "proof.bin", Size: 14, SHA256: hex.EncodeToString(old[:])}}
	_, err := Prepare(t.Context(), "request_1", ManifestDigest(files), []filetransfer.Source{{Basename: "proof.bin", Size: 14, SHA256: old, Reader: bytes.NewReader([]byte("different data"))}}, bytes.Repeat([]byte{8}, 32), directory)
	if err == nil {
		t.Fatal("changed source accepted")
	}
	entries, readErr := os.ReadDir(directory)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("ciphertext snapshot leaked after failure: %d, %v", len(entries), readErr)
	}
}

func TestStoredPullEmptyFile(t *testing.T) {
	prepared, files, key, manifest := storedFixture(t, nil)
	data, err := prepared.ReadPart(0)
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; _, _ = w.Write(data) }))
	defer server.Close()
	root := t.TempDir()
	service, durable := openReceiver(t, root)
	defer durable.Close()
	if err := Pull(t.Context(), pullConfig(prepared, files, key, manifest, service, server.URL)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "Inbox", "proof.bin"))
	if err != nil || info.Size() != 0 || requests != 1 {
		t.Fatalf("empty encrypted file: info=%v requests=%d err=%v", info, requests, err)
	}
}
