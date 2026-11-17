package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/cli/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/cli/internal/storedfiles"
	wire "github.com/pinksaucepasta/paperboat/shared/storedfiles"
	"github.com/pinksaucepasta/paperboat/shared/teambucket"
	"github.com/spf13/cobra"
)

// The command must reach the stored flow for an offline machine, seal the key to
// every recipient machine and return after upload without polling for acceptance.
func TestSendOfflineStoresCiphertextForEveryRecipientMachine(t *testing.T) {
	_, configPath := localDaemonReadinessTestConfig(t, true)
	t.Setenv("PAPERBOAT_TERMINAL_SESSION_ID", "")
	plaintext := []byte("private offline file bytes that must never enter the bucket as plaintext")
	file := filepath.Join(filepath.Dir(configPath), "offline.bin")
	if err := os.WriteFile(file, plaintext, 0o600); err != nil {
		t.Fatal(err)
	}
	firstKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var request api.TeamInboxRequest
	var metadata wire.Metadata
	var uploaded []byte
	acknowledged := false
	bucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodPut || r.Header.Get("X-Signed-Upload") != "required" {
			t.Error("missing signed upload method or header")
			w.WriteHeader(400)
			return
		}
		var readErr error
		uploaded, readErr = io.ReadAll(r.Body)
		if readErr != nil {
			t.Error(readErr)
			w.WriteHeader(500)
			return
		}
		if len(metadata.Parts) != 1 || r.ContentLength != metadata.Parts[0].Bytes {
			t.Error("upload length differs from presigned part")
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(200)
	}))
	defer bucket.Close()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/v1/machines":
			machine := api.UserMachine{ID: "machine_destination", Alias: "offline-recipient", Platform: "linux", State: "offline", Online: false, Capabilities: api.MachineCapabilities{FileReceive: api.MachineCapability{Configured: true, Observed: false}}}
			writeAPIData(t, w, api.UserMachinePage{Items: []api.UserMachine{machine}, Pagination: api.Pagination{Limit: 200, Total: 1}})
		case r.URL.Path == "/v1/team-inbox/requests":
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			files := make([]wire.File, len(request.Files))
			for i, file := range request.Files {
				files[i] = wire.File{Basename: file.Basename, Size: file.Size, SHA256: file.SHA256}
			}
			request.ManifestDigest = storedfiles.ManifestDigest(files)
			request.Status, request.TeamID, request.Stored = "pending", "team_1", true
			writeAPIData(t, w, request)
		case strings.HasSuffix(r.URL.Path, "/stored/recipients"):
			writeAPIData(t, w, map[string]any{"recipients": []wire.Recipient{{MachineID: "machine_destination", HostPublic: firstKey.PublicKey().Bytes()}, {MachineID: "machine_other", HostPublic: secondKey.PublicKey().Bytes()}}})
		case strings.HasSuffix(r.URL.Path, "/stored/metadata"):
			if err := json.NewDecoder(r.Body).Decode(&metadata); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			writeAPIData(t, w, map[string]bool{"stored": true})
		case strings.HasSuffix(r.URL.Path, "/stored/upload-urls"):
			writeAPIData(t, w, wire.URLs{URLs: []teambucket.Authority{{URL: bucket.URL + "/encrypted", Headers: map[string]string{"X-Signed-Upload": "required"}}}})
		case strings.HasSuffix(r.URL.Path, "/stored/uploaded"):
			acknowledged = true
			writeAPIData(t, w, map[string]bool{"uploaded": true})
		default:
			t.Errorf("offline send reached unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer backend.Close()
	writeTestProfile(t, filepath.Dir(configPath), configPath, backend.URL)
	state, err := identity.Open(identity.Config{StateRoot: os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT")})
	if err != nil {
		t.Fatal(err)
	}
	registration, err := state.Registration()
	if err != nil {
		t.Fatal(err)
	}
	registration.ServerURL = backend.URL
	if err := state.SaveRegistration(registration); err != nil {
		t.Fatal(err)
	}
	daemon, _ := localDaemonReadinessServer(t)
	daemonCtx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(daemonCtx) }()
	defer stopLocalDaemonReadinessServer(t, cancel, done)
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		t.Fatal(err)
	}
	waitForCommandSocket(t, paths.SocketPath)
	command := newRootCommand()
	var out, stderr bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&stderr)
	command.SetArgs([]string{"--config", configPath, "--no-customization", "send", file, "--to", "offline-recipient", "--json"})
	commandCtx, finish := context.WithTimeout(t.Context(), 10*time.Second)
	defer finish()
	if err := command.ExecuteContext(commandCtx); err != nil {
		t.Fatalf("offline send: %v (%s)", err, stderr.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if !acknowledged || len(uploaded) == 0 || bytes.Contains(uploaded, plaintext) {
		t.Fatal("stored upload was unacknowledged, empty or plaintext")
	}
	sum := sha256.Sum256(uploaded)
	if len(metadata.Parts) != 1 || metadata.Parts[0].SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("uploaded bytes do not match immutable part manifest")
	}
	if !bytes.Equal(metadata.WriterPublic, state.Current().Public()) || len(metadata.SealedKeys) != 2 {
		t.Fatal("file key was not signed by this machine and sealed to both recipient machines")
	}
	var firstOpened []byte
	for machine, key := range map[string]*ecdh.PrivateKey{"machine_destination": firstKey, "machine_other": secondKey} {
		opened, err := environmente2ee.OpenStoredFileKey(t.Context(), metadata.SealedKeys[machine], state.Current().Public(), key.Bytes(), request.RequestID, request.ManifestDigest, machine)
		if err != nil {
			t.Fatalf("open %s file key: %v", machine, err)
		}
		if firstOpened == nil {
			firstOpened = opened
			defer clear(firstOpened)
		} else {
			if !bytes.Equal(firstOpened, opened) {
				t.Fatal("recipients received different file keys")
			}
			clear(opened)
		}
	}
	var result struct {
		OK   bool `json:"ok"`
		Data struct {
			RequestID string `json:"request_id"`
			Stored    bool   `json:"stored"`
			Uploaded  bool   `json:"uploaded"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.OK || !result.Data.Stored || !result.Data.Uploaded || result.Data.RequestID != request.RequestID {
		t.Fatalf("stored receipt: %s (%v)", out.String(), err)
	}
	if strings.Contains(out.String(), bucket.URL) {
		t.Fatal("receipt exposed a presigned address")
	}
}

func TestStoredSendUploadErrorKeepsSignedAddressPrivate(t *testing.T) {
	_, _ = localDaemonReadinessTestConfig(t, true)
	host, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/redirect?upload-secret=private")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer bucket.Close()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/recipients"):
			writeAPIData(t, w, map[string]any{"recipients": []wire.Recipient{{MachineID: "machine_destination", HostPublic: host.PublicKey().Bytes()}}})
		case strings.HasSuffix(r.URL.Path, "/metadata"):
			writeAPIData(t, w, map[string]bool{"stored": true})
		case strings.HasSuffix(r.URL.Path, "/upload-urls"):
			writeAPIData(t, w, wire.URLs{URLs: []teambucket.Authority{{URL: bucket.URL + "/ciphertext?upload-secret=private"}}})
		default:
			t.Error("failed upload was marked uploaded")
			w.WriteHeader(500)
		}
	}))
	defer backend.Close()
	content := []byte("file bytes")
	hash := sha256.Sum256(content)
	files := []wire.File{{Basename: "file.bin", Size: int64(len(content)), SHA256: hex.EncodeToString(hash[:])}}
	request := api.TeamInboxRequest{RequestID: "request_1", ManifestDigest: storedfiles.ManifestDigest(files), Stored: true}
	client := api.New(backend.URL, config.Credential{AccessToken: "test-access", ExpiresAt: time.Now().Add(time.Hour)}, backend.Client())
	err = sendStoredFiles(t.Context(), &cobra.Command{}, client, request, []filetransfer.Source{{Basename: "file.bin", Size: int64(len(content)), SHA256: hash, Reader: bytes.NewReader(content)}}, "recipient", true)
	if err == nil || !strings.Contains(err.Error(), "upload stored file part 1") || strings.Contains(err.Error(), "upload-secret") || strings.Contains(err.Error(), bucket.URL) {
		t.Fatalf("upload error lost the step or exposed the signed address: %v", err)
	}
}
