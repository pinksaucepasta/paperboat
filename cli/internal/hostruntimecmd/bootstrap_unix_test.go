//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/enrollment"
)

func TestResolveUserShellUsesOverrideAndEnvironment(t *testing.T) {
	shell := filepath.Join(t.TempDir(), "shell")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	canonicalShell, err := filepath.EvalSymlinks(shell)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveUserShell(shell, func(string) string { return "/bin/false" })
	if err != nil || got != canonicalShell {
		t.Fatalf("override shell=%q err=%v", got, err)
	}
	got, err = resolveUserShell("", func(name string) string {
		if name == "SHELL" {
			return shell
		}
		return ""
	})
	if err != nil || got != canonicalShell {
		t.Fatalf("detected shell=%q err=%v", got, err)
	}
	if _, err := resolveUserShell("relative-shell", nil); err == nil {
		t.Fatal("relative shell override was accepted")
	}
}

func TestCanonicalUserHomeAndRemovedWorkspaceOverride(t *testing.T) {
	home, err := canonicalUserHome()
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if home != filepath.Clean(want) {
		t.Fatalf("home=%q want=%q", home, want)
	}
	if err := runBootstrap(context.Background(), []string{"--workspace", t.TempDir()}, strings.NewReader(""), io.Discard, io.Discard); err == nil {
		t.Fatal("removed workspace override was accepted")
	}
}

func TestReportInstallationFailureUsesSignedPersistedIdentity(t *testing.T) {
	stateRoot := enrolledStateRoot(t, "helper_failure", "env_failure")
	var received bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/machine-installation-failures" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer identity-") || r.Header.Get("X-Paperboat-Machine-Proof") == "" {
			t.Errorf("request path=%q auth=%q proof=%q", r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Paperboat-Machine-Proof"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		received = true
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	material := bootstrap.Material{PairingID: "ume_failure", EnvironmentID: "env_failure", HelperID: "helper_failure", ControlURL: server.URL}
	if err := reportInstallationFailureWithClient(context.Background(), material, stateRoot, "service_install", server.Client()); err != nil {
		t.Fatal(err)
	}
	if !received {
		t.Fatal("failure report was not received")
	}
}

func TestReportArtifactFailureUsesOneTimeEnrollmentCredential(t *testing.T) {
	var received bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/machine-installation-failures" || r.Header.Get("Authorization") != "Bearer one-time-enrollment" || r.Header.Get("X-Paperboat-Machine-Proof") != "" {
			t.Errorf("request path=%q auth=%q proof=%q", r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Paperboat-Machine-Proof"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		received = true
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	material := bootstrap.Material{PairingID: "ume_artifact", EnvironmentID: "env_artifact", HelperID: "helper_artifact", EnrollmentID: "henr_artifact", EnrollmentCredential: "one-time-enrollment", ControlURL: server.URL}
	if err := reportInstallationFailureWithEnrollmentCredentialClient(context.Background(), material, "artifact_verification", server.Client()); err != nil {
		t.Fatal(err)
	}
	if !received {
		t.Fatal("artifact failure report was not received")
	}
}

func TestRemoveNewEnrollmentCredentialsPreservesDurableState(t *testing.T) {
	stateRoot := enrolledStateRoot(t, "helper_cleanup", "env_cleanup")
	durable := filepath.Join(stateRoot, "sessions.db")
	if err := os.WriteFile(durable, []byte("durable"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeNewEnrollmentCredentials(stateRoot); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"runtime-identity.json"} {
		if _, err := os.Lstat(filepath.Join(stateRoot, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("credential %s remains: %v", name, err)
		}
	}
	if body, err := os.ReadFile(durable); err != nil || string(body) != "durable" {
		t.Fatalf("durable state changed: body=%q err=%v", body, err)
	}
}

func enrolledStateRoot(t *testing.T, helperID, environmentID string) string {
	t.Helper()
	root := t.TempDir()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": enrollment.RuntimeIdentity{HelperID: helperID, MachineID: "machine_" + environmentID, EnvironmentID: environmentID, Credential: "identity-" + strings.Repeat("x", 40), ExpiresAt: time.Now().UTC().Add(time.Hour)}})
	}))
	defer server.Close()
	client, err := enrollment.NewClient(server.Client().Transport, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Enroll(context.Background(), enrollment.Config{ControlURL: server.URL, StateRoot: root, EnrollmentCredential: strings.Repeat("e", 40)}); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestBootstrapHealthMatchesSignedArtifactVersion(t *testing.T) {
	response := func(status int, body string) *http.Response {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
	}
	healthMatches := func(response *http.Response, version string) bool {
		matches, err := checkBootstrapHealthMatches(response, version)
		return err == nil && matches
	}
	if !healthMatches(response(http.StatusOK, `{"live":true,"version":"2026.07.26","capabilities":{},"checked_at":"2026-07-26T00:00:00Z"}`), "2026.07.26") {
		t.Fatal("matching signed artifact version was not ready")
	}
	if !healthMatches(response(http.StatusOK, `{"live":true}`), "2026.07.26") {
		t.Fatal("stable liveness response was not ready")
	}
	for name, candidate := range map[string]*http.Response{
		"legacy version": response(http.StatusOK, `{"live":true,"version":"legacy","capabilities":{},"checked_at":"2026-07-26T00:00:00Z"}`),
		"not live":       response(http.StatusOK, `{"live":false,"version":"2026.07.26","capabilities":{},"checked_at":"2026-07-26T00:00:00Z"}`),
		"empty object":   response(http.StatusOK, `{}`),
		"oversized":      response(http.StatusOK, `{"live":true,"padding":"`+strings.Repeat("x", 64<<10)+`"}`),
		"trailing data":  response(http.StatusOK, `{"live":true,"version":"2026.07.26","capabilities":{},"checked_at":"2026-07-26T00:00:00Z"} {}`),
		"wrong status":   response(http.StatusServiceUnavailable, `{}`),
	} {
		t.Run(name, func(t *testing.T) {
			if healthMatches(candidate, "2026.07.26") {
				t.Fatal("invalid health response accepted")
			}
		})
	}
}

func TestWorkerGenerationRequiresStrictDurableState(t *testing.T) {
	root := t.TempDir()
	runtimeRoot := filepath.Join(root, "runtime")
	if err := os.Mkdir(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(runtimeRoot, "worker-boot.json")
	valid := `{"schema":"paperboat.worker-boot/v1","os_boot_id":"boot-1","generation":4,"started_at":"2026-07-26T00:00:00Z"}`
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if generation, err := readWorkerGeneration(root); err != nil || generation != 4 {
		t.Fatalf("generation = %d err=%v", generation, err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimSuffix(valid, "}")+`,"unexpected":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if generation, err := readWorkerGeneration(root); err == nil {
		t.Fatalf("hostile state generation = %d was accepted", generation)
	}
}

func TestServerHeartbeatReadinessBindsVersionGenerationAndFreshness(t *testing.T) {
	root := t.TempDir()
	runtimeRoot := filepath.Join(root, "runtime")
	if err := os.Mkdir(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(generation uint64, version string) {
		body, err := json.Marshal(map[string]any{"schema": "paperboat.server-heartbeat/v1", "worker_generation": generation, "reporter_version": version, "accepted_at": "2026-07-26T00:00:00Z"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runtimeRoot, "server-heartbeat.json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(5, "2026.07.25.4")
	accepted := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	heartbeatReady := func(version string, generation uint64, since time.Time) bool {
		ready, err := checkServerHeartbeatReady(root, version, generation, since)
		return err == nil && ready
	}
	if !heartbeatReady("2026.07.25.4", 5, accepted) {
		t.Fatal("new generation heartbeat was not ready")
	}
	if heartbeatReady("2026.07.25.5", 5, accepted) || heartbeatReady("2026.07.25.4", 4, accepted) || heartbeatReady("2026.07.25.4", 5, accepted.Add(time.Second)) {
		t.Fatal("stale or wrong-version heartbeat was accepted")
	}
}

func TestBootstrapReadinessReusesCurrentWorkerOnlyWithFreshReceipt(t *testing.T) {
	root, now := t.TempDir(), time.Now().UTC()
	if err := os.Mkdir(filepath.Join(root, "runtime"), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name string, value any) {
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "runtime", name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("worker-boot.json", map[string]any{"schema": "paperboat.worker-boot/v1", "os_boot_id": "boot", "generation": 5, "started_at": now.Add(-time.Hour)})
	write("server-heartbeat.json", map[string]any{"schema": "paperboat.server-heartbeat/v1", "worker_generation": 5, "reporter_version": "2026.07.25.4", "accepted_at": now})
	ready := func(minimum uint64, since time.Time) bool {
		response := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"live":true}`))}
		ok, err := checkBootstrapWorkerReady(context.Background(), response, root, "2026.07.25.4", minimum, since, false)
		return err == nil && ok
	}

	var truncate atomic.Bool
	truncate.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if truncate.Load() {
			connection, buffer, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer connection.Close()
			_, _ = buffer.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\nConnection: close\r\n\r\n{\"live\":")
			_ = buffer.Flush()
			return
		}
		_, _ = io.WriteString(w, `{"live":true}`)
	}))
	defer server.Close()
	response, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	available, err := checkBootstrapWorkerReady(context.Background(), response, root, "2026.07.25.4", 5, now, false)
	if available || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated readiness cause lost: ready=%t type=%T", available, err)
	}
	truncate.Store(false)
	response, err = server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	available, err = checkBootstrapWorkerReady(context.Background(), response, root, "2026.07.25.4", 5, now, false)
	if !available || err != nil {
		t.Fatalf("fresh HTTP readiness did not recover: %T", err)
	}
	if !ready(5, now) {
		t.Fatal("resumed healthy worker was rejected")
	}
	if ready(6, now) {
		t.Fatal("fresh install accepted an old worker")
	}
	if ready(5, now.Add(time.Second)) {
		t.Fatal("retry accepted stale server readiness")
	}
}

func TestFailedInstallationReportPreservesRecoveryCredential(t *testing.T) {
	root := enrolledStateRoot(t, "helper_preserve", "env_preserve")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusConflict) }))
	defer server.Close()
	material := bootstrap.Material{ControlURL: server.URL, HelperID: "helper_preserve", EnvironmentID: "env_preserve", PairingID: "ume_preserve"}
	err := failBootstrapInstallation(context.Background(), errors.New("not ready"), material, root, "service_readiness")
	status := structuralHTTPStatus(err)
	if err == nil || status != http.StatusConflict {
		t.Fatalf("failure report = %v", err)
	}
	if _, err := enrollment.LoadRuntimeIdentityForRenewal(root, time.Now().UTC()); err != nil {
		t.Fatalf("recovery credential removed: %v", err)
	}
}

func TestInstallHelperCommandTargetsCanonicalWorkerAndRestoresPreviousLink(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	commandPath := filepath.Join(directory, "pb")
	previous := "/tmp/previous-paperboat"
	if err := os.Symlink(previous, commandPath); err != nil {
		t.Fatal(err)
	}
	installation, err := installWorkerCommand(directory, systemWorkerExecutable())
	if err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(commandPath); err != nil || target != systemWorkerExecutable() {
		t.Fatalf("target=%q err=%v", target, err)
	}
	if err := installation.Rollback(); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(commandPath); err != nil || target != previous {
		t.Fatalf("restored target=%q err=%v", target, err)
	}
}

func TestInstallHelperCommandReplacesAndRestoresRegularExecutable(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	commandPath := filepath.Join(directory, "pb")
	if err := os.WriteFile(commandPath, []byte("user-owned"), 0o700); err != nil {
		t.Fatal(err)
	}
	installation, err := installWorkerCommand(directory, systemWorkerExecutable())
	if err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(commandPath); err != nil || target != systemWorkerExecutable() {
		t.Fatalf("target=%q err=%v", target, err)
	}
	if err := installation.Rollback(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(commandPath)
	if err != nil || string(contents) != "user-owned" {
		t.Fatalf("contents=%q err=%v", contents, err)
	}
}

func TestInstallHelperCommandCommitsRegularExecutableReplacement(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	commandPath := filepath.Join(directory, "pb")
	if err := os.WriteFile(commandPath, []byte("old executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	installation, err := installWorkerCommand(directory, systemWorkerExecutable())
	if err != nil {
		t.Fatal(err)
	}
	if err := installation.Commit(); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(commandPath); err != nil || target != systemWorkerExecutable() {
		t.Fatalf("target=%q err=%v", target, err)
	}
	matches, err := filepath.Glob(filepath.Join(directory, ".pb-backup-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("backups=%v err=%v", matches, err)
	}
}

func TestRemoveSystemHelperCommandOnlyRemovesCanonicalSymlink(t *testing.T) {
	for name, target := range map[string]struct {
		target  string
		removed bool
	}{
		"canonical": {target: systemWorkerExecutable(), removed: true},
		"unrelated": {target: "/tmp/user-helper", removed: false},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			directory := filepath.Join(home, ".local", "bin")
			if err := os.MkdirAll(directory, 0o755); err != nil {
				t.Fatal(err)
			}
			commandPath := filepath.Join(directory, "pb")
			if err := os.Symlink(target.target, commandPath); err != nil {
				t.Fatal(err)
			}
			if err := removeSystemWorkerCommand(); err != nil {
				t.Fatal(err)
			}
			_, err := os.Lstat(commandPath)
			if target.removed && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("canonical command remains: %v", err)
			}
			if !target.removed && err != nil {
				t.Fatalf("unrelated command removed: %v", err)
			}
		})
	}
}

func structuralHTTPStatus(err error) int {
	var status interface{ DiagnosticStatus() int }
	if errors.As(err, &status) {
		return status.DiagnosticStatus()
	}
	return 0
}
