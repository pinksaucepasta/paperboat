// net-blip-refusal rotates and restores a throwaway CLI endpoint's discovery key
// through the ordinary registration API while pb.exe's network is blocked.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"golang.org/x/crypto/curve25519"
)

type registrar interface {
	RegisterPeerNetwork(context.Context, api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error)
}

// restore runs even when the caller's request context has expired. It restores
// public identity, not the monotonic revocation floor (which must never roll back).
func rotate(client registrar, original api.PeerNetworkRegistration) (raised, restored uint64, err error) {
	request := original
	var private [32]byte
	defer clear(private[:])
	if _, err = rand.Read(private[:]); err != nil {
		return
	}
	public, e := curve25519.X25519(private[:], curve25519.Basepoint)
	if e != nil {
		err = e
		return
	}
	request.DiscoPublicKey = base64.RawURLEncoding.EncodeToString(public)
	request.OperationID = fmt.Sprintf("f2148_raise_%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, e := client.RegisterPeerNetwork(ctx, request)
	if e != nil {
		err = fmt.Errorf("rotate disco key: %w", e)
		return
	}
	raised = result.KeyGeneration
	request = original
	request.ExpectedKeyGeneration = raised
	request.OperationID = fmt.Sprintf("f2148_restore_%d", time.Now().UnixNano())
	restoreCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	result, e = client.RegisterPeerNetwork(restoreCtx, request)
	if e != nil {
		err = fmt.Errorf("restore original disco key at generation %d: %w", raised, e)
		return
	}
	restored = result.KeyGeneration
	return
}

func run() error {
	endpoint := flag.String("endpoint", "", "exact throwaway CLI endpoint id")
	server := flag.String("server", "", "server URL from the runner registration")
	verify := flag.Bool("verify", false, "report the recovered cached registration without rotating")
	flag.Parse()
	if *endpoint == "" {
		return errors.New("exact --endpoint required")
	}
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	if *server == "" {
		return errors.New("--server from runner registration required")
	}
	cfg.ServerURL = *server
	store, err := config.ProfileStoreFor(cfg)
	if err != nil {
		return err
	}
	profile, err := store.Load(cfg.ServerURL)
	if err != nil {
		return err
	}
	if profile.CLIClientSessionID != *endpoint {
		return errors.New("endpoint does not match this signed-in CLI")
	}
	token, err := store.Secrets.Get(profile.AccessSecretRef)
	if err != nil {
		return err
	}
	cached, err := store.LoadPeerNetworkConfiguration(cfg.ServerURL, profile.Account.ID, *endpoint, 4<<20)
	if err != nil {
		return err
	}
	parts := strings.Split(cached, ".")
	if len(parts) != 3 {
		return errors.New("no cached peer configuration; run exec first")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	var document tailnet.NetworkConfiguration
	if err = json.Unmarshal(body, &document); err != nil {
		return err
	}
	self := document.Self
	if self.EndpointID != *endpoint || self.AccountID != profile.Account.ID || self.Role != "cli" {
		return errors.New("cached configuration belongs to another endpoint")
	}
	if *verify {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"endpoint_id": *endpoint, "key_generation": self.KeyGeneration, "config_generation": document.Generation})
	}
	client := api.New(cfg.ServerURL, config.Credential{AccessToken: token, TokenType: "Bearer"}, &http.Client{Timeout: 30 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	before, err := client.PeerNetworkConfiguration(ctx, api.PeerNetworkConfigurationRequest{
		OperationID: fmt.Sprintf("f2148_snapshot_%d", time.Now().UnixNano()), Authority: api.PeerNetworkAuthorityUntilRevoked,
	})
	if err != nil {
		return err
	}
	parts = strings.Split(before.Configuration, ".")
	if len(parts) != 3 {
		return errors.New("invalid configuration response")
	}
	body, err = base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	var baseline struct {
		Generation uint64 `json:"generation"`
	}
	if err = json.Unmarshal(body, &baseline); err != nil || baseline.Generation == 0 {
		return errors.New("invalid configuration generation")
	}
	raised, restored, err := rotate(client, api.PeerNetworkRegistration{
		WireGuardPublicKey: self.WireGuardPublicKey, DiscoPublicKey: self.DiscoPublicKey,
		ExpectedKeyGeneration: self.KeyGeneration, QUICCertificateFingerprint: self.QUICCertificateFingerprint,
	})
	receipt := map[string]any{
		"endpoint_id": *endpoint, "key_generation_before": self.KeyGeneration,
		"key_generation_rotated": raised, "key_generation_restored": restored,
		"original_disco_restored": restored != 0, "floor_rollback": false,
		"config_generation_before_rotation": baseline.Generation,
		"floor_evidence":                    "registration trigger: floor becomes prior config generation + 1; pb.exe remains blocked",
		"custody_updated":                   false,
	}
	if raised != 0 {
		receipt["floor_after_rotation"] = baseline.Generation + 1
	}
	if restored != 0 {
		receipt["floor_after_identity_restore"] = baseline.Generation + 2
	}
	defer func() { _ = json.NewEncoder(os.Stdout).Encode(receipt) }()
	if err != nil {
		return err
	}
	// Public keys are back to the original values. Keep custody's key generation
	// in sync so the existing authority can apply the next signed configuration.
	if err = store.UpdatePeerNetworkState(cfg.ServerURL, profile.Account.ID, *endpoint, func(state *config.PeerNetworkState) error {
		if state.KeyGeneration != self.KeyGeneration {
			return errors.New("custody changed during refusal setup")
		}
		state.KeyGeneration = restored
		return nil
	}); err != nil {
		return err
	}
	receipt["custody_updated"] = true
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
