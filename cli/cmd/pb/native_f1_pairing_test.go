//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	commandpkg "github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	runtimeconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

// This live acceptance test uses the configured account through the production
// client factory. It creates only its own enrollment fixtures and removes the
// unused fixture machine; the enrolled device never gets reset or reinstalled.
func TestNativeF1PairingTokenRejections(t *testing.T) {
	if os.Getenv("PAPERBOAT_NATIVE_F1") != "1" {
		t.Skip("explicit real-device F1 acceptance only")
	}
	binary := os.Getenv("PAPERBOAT_F1_BINARY")
	if !filepath.IsAbs(binary) {
		t.Fatal("PAPERBOAT_F1_BINARY must name the candidate")
	}
	if buildinfo.Version == "dev" || buildinfo.Commit == "" {
		t.Fatal("native acceptance must use the stamped candidate test build")
	}
	version, err := exec.Command(binary, "--version").Output()
	if err != nil || !strings.Contains(string(version), buildinfo.Version) {
		t.Fatal("candidate does not match this committed test build")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	flags := flag.NewFlagSet("native-f1", flag.ContinueOnError)
	flags.String("config", "", "")
	flags.String("server", "", "")
	flags.String("workspace", "personal", "")
	actor := commandpkg.NewContext(flags)
	actor.Context = ctx
	actor.Writer = io.Discard
	actor.ErrWriter = io.Discard
	dependencies, err := buildDeps(actor)
	if err != nil {
		t.Fatal("load configured test-device account")
	}
	client, err := newWorkspaceAPIClient(actor, dependencies.cfg.ServerURL, dependencies.auth)
	if err != nil {
		t.Fatal("initialize configured account client")
	}
	state, err := runtimeconfig.DefaultStateRoot(os.Getenv)
	if err != nil {
		t.Fatal("locate enrolled runtime")
	}
	store, err := identity.Open(identity.Config{StateRoot: state})
	if err != nil {
		t.Fatal("open existing machine identity")
	}
	before, err := store.Registration()
	if err != nil {
		t.Fatal("test requires an existing enrolled device")
	}
	fixture := nativeF1PairingConfig(t, dependencies.cfg.ServerURL)
	used := nativeF1UsedEnrollment(t, ctx, client, fixture, before.MachineID)
	revoked, err := client.StartMachineEnrollment(ctx, newIdempotencyKey(), "posix")
	if err != nil {
		t.Fatal("create revocation fixture")
	}
	if err := client.CancelMachineEnrollment(ctx, revoked.ID, newIdempotencyKey()); err != nil {
		t.Fatal("cancel revocation fixture")
	}
	expired := nativeF1ExpiredToken(t, os.Getenv("PAPERBOAT_F1_EXPIRED_TOKEN_FILE"))
	cases := []struct {
		name, token, message string
		outcome              error
	}{
		{"expired", expired, "token expired", bootstrap.ErrEnrollmentTokenExpired},
		{"used", used, "already used", bootstrap.ErrEnrollmentTokenUsed},
		{"revoked", revoked.BootstrapToken, "revoked or invalid", bootstrap.ErrEnrollmentTokenInvalid},
		// Odd shell metadata is an unsupported command shape on Unix; no Windows enrollment is created.
		{"platform", "1" + strings.Repeat("Z", 25), "operating system", bootstrap.ErrPairingPlatformUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := fixture
			probe.EnrollmentToken = tc.token
			probe.Verifier = newIdempotencyKey() + newIdempotencyKey()
			if _, err := bootstrap.CreatePairing(ctx, probe); !errors.Is(err, tc.outcome) {
				t.Fatal("live producer did not reject fixture before invoking CLI")
			}
			root := t.TempDir()
			tokenFile := filepath.Join(root, "token")
			if err := os.WriteFile(tokenFile, []byte(tc.token), 0600); err != nil {
				t.Fatal("write private fixture token")
			}
			runtimeRoot := filepath.Join(root, "runtime")
			command := exec.CommandContext(ctx, binary, "pair", "--json", "--server", fixture.ServerURL,
				"--enrollment-token-file", tokenFile, "--state-root", runtimeRoot, "--name", "f1-rejected")
			output, runErr := command.Output()
			var receipt struct {
				OK    bool `json:"ok"`
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if runErr == nil || json.Unmarshal(output, &receipt) != nil || receipt.OK || !strings.Contains(receipt.Error.Message, tc.message) {
				t.Fatal("CLI did not return the specific safe rejection")
			}
			rejected, err := identity.Open(identity.Config{StateRoot: runtimeRoot})
			if err != nil {
				t.Fatal("inspect rejected local identity")
			}
			if _, err := rejected.Registration(); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("rejected pairing installed a registration")
			}
			public := base64.RawURLEncoding.EncodeToString(rejected.Current().Public())
			if _, err := bootstrap.LoadResume(runtimeRoot, fixture.ServerURL, public, tc.token, "f1-rejected", time.Now()); !errors.Is(err, bootstrap.ErrResumeNotFound) {
				t.Fatal("fresh rejection left an unfinished journal")
			}
			if _, err := os.Stat(tokenFile); err != nil {
				t.Fatal("rejection consumed input token")
			}
			after, err := store.Registration()
			if err != nil || after.MachineID != before.MachineID || after.PublicKeyID != before.PublicKeyID || after.InstallationGeneration != before.InstallationGeneration {
				t.Fatal("negative pairing replaced existing enrolled identity")
			}
		})
	}
}

func nativeF1PairingConfig(t *testing.T, server string) bootstrap.Config {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal("generate fixture identity")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal("resolve fixture workspace")
	}
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal("generate fixture name")
	}
	return bootstrap.Config{ServerURL: server, Alias: "f1-token-" + hex.EncodeToString(nonce),
		WorkspaceRoot: root, Verifier: newIdempotencyKey() + newIdempotencyKey(),
		PublicIdentityKey: base64.RawURLEncoding.EncodeToString(public)}
}

func nativeF1UsedEnrollment(t *testing.T, ctx context.Context, client *api.Client, config bootstrap.Config, protected string) string {
	t.Helper()
	enrollment, err := client.StartMachineEnrollment(ctx, newIdempotencyKey(), "posix")
	if err != nil {
		t.Fatal("create used-token fixture")
	}
	config.EnrollmentToken = enrollment.BootstrapToken
	pairing, err := bootstrap.CreatePairing(ctx, config)
	if err != nil {
		t.Fatal("claim used-token fixture")
	}
	material, err := bootstrap.WaitForMaterial(ctx, config, pairing.ExpiresAt, 100*time.Millisecond)
	if err != nil || material.UserMachineID == "" || material.UserMachineID == protected {
		t.Fatal("used-token fixture did not receive a separate test machine")
	}

	machines, err := client.ListUserMachines(ctx)
	if err != nil {
		t.Fatal("verify fixture machine authority")
	}
	matched := false
	for _, machine := range machines {
		if machine.ID == material.UserMachineID && machine.Alias == config.Alias {
			matched = true
		}
	}
	if !matched {
		t.Fatal("fixture machine did not match its exact allocated alias")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if err := client.DeleteUserMachine(cleanup, material.UserMachineID); err != nil {
			t.Error("remove only the used-token fixture machine")
		}
	})
	return enrollment.BootstrapToken
}

func nativeF1ExpiredToken(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 4096 {
		t.Fatal("expired fixture must be an owner-only regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read private expired fixture")
	}
	defer clear(data)
	var fixture struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&fixture) != nil || decoder.Decode(new(any)) != io.EOF || len(fixture.Token) < 26 || fixture.ExpiresAt.IsZero() || !time.Now().After(fixture.ExpiresAt) {
		t.Fatal("fixture is not a definitively expired enrollment")
	}
	return fixture.Token
}
