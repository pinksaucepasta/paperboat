//go:build darwin || linux || windows

package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFileSHA256IsTheHashOfTheBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pb")
	if err := os.WriteFile(path, []byte("pb build"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("pb build"))
	if got := fileSHA256(path); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash = %q", got)
	}
	if got := fileSHA256(path + ".missing"); got != "" {
		t.Fatalf("a missing file reported %q", got)
	}
	if got := executableSHA256(); len(got) != 64 {
		t.Fatalf("test binary hash = %q", got)
	}
}

// The heartbeat that carries reporter_version carries the executable's hash with it.
func TestObservationReportsTheExecutableHash(t *testing.T) {
	var body struct {
		ReporterVersion string `json:"reporter_version"`
		ReporterSHA256  string `json:"reporter_sha256"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	}))
	defer server.Close()
	hash := hex.EncodeToString(make([]byte, 32))
	s := &runtimeObservationSender{endpoint: server.URL, machineID: "machine_a", environmentID: "env_a", reporterVersion: "2026.11.1.1", reporterSHA256: hash, installationGeneration: 7,
		tokens: livenessObservationTokenSource{}, proofs: livenessObservationProofSource{}, operationID: func() (string, error) { return "operation_a", nil }, client: server.Client()}
	if err := s.Send(context.Background()); err != nil {
		t.Fatal(err)
	}
	if body.ReporterVersion != "2026.11.1.1" || body.ReporterSHA256 != hash {
		t.Fatalf("reported version %q hash %q", body.ReporterVersion, body.ReporterSHA256)
	}
}
