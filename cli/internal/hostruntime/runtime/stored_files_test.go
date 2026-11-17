//go:build darwin || linux || windows

package runtime

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	clientconfig "github.com/pinksaucepasta/paperboat/cli/internal/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/environmente2ee"
	sender "github.com/pinksaucepasta/paperboat/cli/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/environmentkey"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/filetransfer"
	runtimeidentity "github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/store"
	"github.com/pinksaucepasta/paperboat/cli/internal/storedfiles"
	wire "github.com/pinksaucepasta/paperboat/shared/storedfiles"
	"github.com/pinksaucepasta/paperboat/shared/teambucket"
)

type storedPullTestKeys struct{ material environmentkey.Material }

func (s storedPullTestKeys) Load(context.Context) (environmentkey.Material, error) {
	return s.material, nil
}

type storedPullTestCredential struct{ identity *runtimeidentity.Store }

func (storedPullTestCredential) Token(context.Context) (string, error) {
	return "stored-test-machine-credential", nil
}
func (s storedPullTestCredential) Proof(_ context.Context, operation, method, path string, body []byte) ([]byte, error) {
	return s.identity.MachineProof(operation, method, path, body, time.Now().UTC())
}

// A lost acknowledgement must retry receipt after durable Inbox publication,
// without downloading or publishing again. The real API client must use machine
// proof on both list reads and receipt/download mutations, with no user session.
func TestStoredFileControlProofAndLostReceiptRetry(t *testing.T) {
	root := t.TempDir()
	plaintext := []byte("offline sender may disconnect before these bytes are accepted")
	delivery, prepared, material := storedControlTestDelivery(t, plaintext)
	durable, err := store.Open(t.Context(), store.Config{Root: filepath.Join(root, "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	inbox := filepath.Join(root, "Inbox")
	receiver, err := filetransfer.New(filetransfer.Config{Root: filepath.Join(root, "spool"), Store: durable, LocalMachineID: "machine_recipient", PublishRoot: func() (string, error) { return inbox, nil }})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := runtimeidentity.Open(runtimeidentity.Config{StateRoot: filepath.Join(root, "identity")})
	if err != nil {
		t.Fatal(err)
	}
	counts := &storedControlTestCounts{}
	bucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counts.reads.Add(1)
		if r.Method != http.MethodGet || r.Header.Get("X-Stored-Download") != "required" {
			t.Error("download did not preserve its signed authority")
			w.WriteHeader(400)
			return
		}
		part, err := prepared.ReadPart(0)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write(part)
	}))
	defer bucket.Close()
	backend := storedControlTestServer(t, identity, delivery, bucket.URL, inbox, plaintext, counts)
	defer backend.Close()
	key := identity.Current()
	if err := identity.SaveRegistration(runtimeidentity.Registration{ServerURL: backend.URL, MachineID: "machine_recipient", EnvironmentID: "environment_recipient", PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: inbox, InstallationGeneration: 1, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	client := api.New(backend.URL, clientconfig.Credential{}, backend.Client())
	client.SetMachineAuth(storedPullTestCredential{identity: identity})
	var operations atomic.Int32
	control := &storedFileControl{client: client, bucket: bucket.Client(), keys: storedPullTestKeys{material: material}, receiver: receiver, machine: "machine_recipient", operationID: func() (string, error) { return fmt.Sprintf("stored-test-operation-%d", operations.Add(1)), nil }}
	var lost *api.ResponseLostError
	if err := control.pull(t.Context()); !errors.As(err, &lost) {
		t.Fatalf("lost receipt response was not retained: %v", err)
	}
	if err := control.pull(t.Context()); err != nil {
		t.Fatalf("receipt retry: %v", err)
	}
	if err := control.pull(t.Context()); err != nil {
		t.Fatalf("repeated completed delivery: %v", err)
	}
	if counts.reads.Load() != 1 || counts.downloads.Load() != 1 || counts.lists.Load() != 3 || counts.receipts.Load() != 3 {
		t.Fatalf("reads/downloads/lists/receipts = %d/%d/%d/%d", counts.reads.Load(), counts.downloads.Load(), counts.lists.Load(), counts.receipts.Load())
	}
	entries, err := os.ReadDir(inbox)
	if err != nil || len(entries) != 1 || entries[0].Name() != "proof.bin" {
		t.Fatalf("repeated receipt duplicated publication: %v, %v", entries, err)
	}
}

type storedControlTestCounts struct{ reads, lists, downloads, receipts atomic.Int32 }

func storedControlTestServer(t *testing.T, identity *runtimeidentity.Store, delivery wire.Delivery, bucketURL, inbox string, plaintext []byte, counts *storedControlTestCounts) *httptest.Server {
	t.Helper()
	var seen sync.Map
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || !storedControlTestProof(t, r, body, identity.Current().Public(), &seen) {
			w.WriteHeader(401)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET " + wire.MachinePath:
			counts.lists.Add(1)
			writeStoredControlTestData(t, w, map[string]any{"transfers": []wire.Delivery{delivery}, "future_field": true})
		case "POST " + wire.MachinePath + "/request_1/download-urls":
			counts.downloads.Add(1)
			var in wire.Range
			if json.Unmarshal(body, &in) != nil || in.First != 0 || in.Count != 1 {
				t.Error("download request did not bind exact part range")
				w.WriteHeader(400)
				return
			}
			// The lost receipt uses a fresh connection, so the HTTP transport does
			// not replay it internally before the controller observes the loss.
			w.Header().Set("Connection", "close")
			writeStoredControlTestData(t, w, map[string]any{"urls": []teambucket.Authority{{URL: bucketURL + "/encrypted", Headers: map[string]string{"X-Stored-Download": "required"}}}, "future_field": true})
		case "POST " + wire.MachinePath + "/request_1/received":
			var in struct {
				ManifestDigest string `json:"manifest_digest"`
			}
			if json.Unmarshal(body, &in) != nil || in.ManifestDigest != delivery.ManifestDigest {
				t.Error("receipt did not bind verified manifest")
				w.WriteHeader(400)
				return
			}
			actual, err := os.ReadFile(filepath.Join(inbox, "proof.bin"))
			if err != nil || !bytes.Equal(actual, plaintext) {
				t.Error("receipt preceded byte-equal Inbox publication")
				w.WriteHeader(500)
				return
			}
			if counts.receipts.Add(1) == 1 {
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = connection.Close() // the receipt reached the server; its answer is lost
				return
			}
			writeStoredControlTestData(t, w, map[string]any{"received": true, "future_field": true})
		default:
			t.Errorf("unexpected stored control route %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
}

func storedControlTestDelivery(t *testing.T, plaintext []byte) (wire.Delivery, *storedfiles.Prepared, environmentkey.Material) {
	t.Helper()
	private, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	material := environmentkey.Material{Generation: 1}
	copy(material.Private[:], private.Bytes())
	t.Cleanup(material.Destroy)
	writer, err := runtimeidentity.Open(runtimeidentity.Config{StateRoot: filepath.Join(t.TempDir(), "sender-identity")})
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{31}, 32)
	defer clear(key)
	sum := sha256.Sum256(plaintext)
	files := []wire.File{{Basename: "proof.bin", Size: int64(len(plaintext)), SHA256: hex.EncodeToString(sum[:])}}
	manifest := storedfiles.ManifestDigest(files)
	prepared, err := storedfiles.Prepare(t.Context(), "request_1", manifest, []sender.Source{{Basename: "proof.bin", Size: int64(len(plaintext)), SHA256: sum, Reader: bytes.NewReader(plaintext)}}, key, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := prepared.Close(); err != nil {
			t.Error(err)
		}
	})
	signer := writer.Current()
	sealed, err := environmente2ee.SealStoredFileKey(t.Context(), environmente2ee.StoredFileClaims{RequestID: "request_1", ManifestDigest: manifest, MachineID: "machine_recipient", HostPublic: private.PublicKey().Bytes(), WriterPublic: signer.Public()}, key, signer.Sign)
	if err != nil {
		t.Fatal(err)
	}
	return wire.Delivery{RequestID: "request_1", ManifestDigest: manifest, SourceMachineID: "machine_sender", SenderAccount: "account_sender", Files: files, Metadata: wire.Metadata{Parts: prepared.Parts, WriterPublic: signer.Public(), SealedKeys: map[string][]byte{"machine_recipient": sealed}}}, prepared, material
}

func storedControlTestProof(t *testing.T, r *http.Request, body []byte, public ed25519.PublicKey, seen *sync.Map) bool {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
	var envelope struct {
		Algorithm string `json:"alg"`
		Payload   string `json:"payload"`
		Signature string `json:"signature"`
	}
	if err != nil || json.Unmarshal(raw, &envelope) != nil {
		t.Error("stored request has no machine proof")
		return false
	}
	payload, payloadErr := base64.RawURLEncoding.DecodeString(envelope.Payload)
	signature, signatureErr := base64.RawURLEncoding.DecodeString(envelope.Signature)
	if payloadErr != nil || signatureErr != nil || envelope.Algorithm != "EdDSA" || !ed25519.Verify(public, payload, signature) {
		t.Error("stored request has an invalid machine signature")
		return false
	}
	var claims struct {
		MachineID   string `json:"machine_id"`
		OperationID string `json:"operation_id"`
		Method      string `json:"method"`
		Path        string `json:"path"`
		BodySHA256  string `json:"body_sha256"`
	}
	hash := sha256.Sum256(body)
	if json.Unmarshal(payload, &claims) != nil || claims.MachineID != "machine_recipient" || claims.OperationID == "" || claims.OperationID != r.Header.Get("Idempotency-Key") || claims.Method != r.Method || claims.Path != r.URL.Path || claims.BodySHA256 != base64.RawURLEncoding.EncodeToString(hash[:]) || r.Header.Get("Authorization") != "Bearer stored-test-machine-credential" || r.Header.Get("X-Paperboat-Machine-Identity") != "stored-test-machine-credential" {
		t.Error("stored proof did not bind the exact machine, operation, method, path and body")
		return false
	}
	if _, exists := seen.LoadOrStore(claims.OperationID, true); exists {
		t.Error("independent stored requests reused an operation ID")
		return false
	}
	return true
}

func writeStoredControlTestData(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data, "future_envelope_field": true}); err != nil {
		t.Error(err)
	}
}
