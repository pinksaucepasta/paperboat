package environmentmanager

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

func TestENVDecodeIntegrityRetainsParserCauseAndRecovers(t *testing.T) {
	keys, err := environmente2ee.NewVaultKeys()
	if err != nil {
		t.Fatal("keys fixture failed")
	}
	defer keys.Clear()
	payload, err := keys.MarshalBinary()
	if err != nil {
		t.Fatal("payload fixture failed")
	}
	defer clear(payload)
	raw, head, err := environmente2ee.SealPasswordVault(t.Context(), environmente2ee.VaultHead{Issuer: "https://control.example", AccountID: "account_1", Generation: 1}, environmente2ee.DocumentID{}, []byte("fixture password"), payload)
	if err != nil {
		t.Fatal("vault fixture sealing failed")
	}
	defer clear(raw)
	validVault := api.PasswordVaultState{Issuer: head.Issuer, AccountID: head.AccountID, Generation: head.Generation, DocumentID: head.ID.String(), Envelope: base64.RawURLEncoding.EncodeToString(raw)}
	sealed, err := environmente2ee.SealVaultScope(t.Context(), environmente2ee.VaultScopeClaims{WorkspaceID: "personal", Issuer: head.Issuer, OwnerKind: "personal", OwnerID: head.AccountID, KeyEpoch: 1, Revision: 1, Previous: make([]byte, 32), WriterAccount: head.AccountID, WriterVaultGeneration: 1}, keys.PersonalKey, keys.WriterSeed, map[string][]byte{"NAME": []byte("fixture")})
	if err != nil {
		t.Fatal("scope fixture sealing failed")
	}
	signer := ed25519.NewKeyFromSeed(keys.WriterSeed)
	defer clear(signer)
	validScope := api.VaultScopeState{WorkspaceID: "personal", OwnerKind: "personal", OwnerID: head.AccountID, KeyEpoch: 1, Revision: 1, DocumentID: sealed.ID.String(), Envelope: base64.RawURLEncoding.EncodeToString(sealed.Raw), WriterPublic: base64.RawURLEncoding.EncodeToString(signer.Public().(ed25519.PublicKey))}
	const canary = "PRIVATE_CANARY!"
	var mode atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		switch mode.Load() {
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":{"code":"service_unavailable","message":"PRIVATE_CANARY"}}`)
			return
		case 3:
			w.Header().Set("Content-Length", "1000")
			io.WriteString(w, `{"data":`)
			return
		}
		var data any
		if strings.Contains(r.URL.Path, "environment-vault") {
			state := validVault
			if mode.Load() == 0 {
				state.Envelope = canary
			}
			data = state
		} else {
			state := validScope
			if mode.Load() == 0 {
				state.WriterPublic = canary
			}
			data = state
		}
		if err := json.NewEncoder(w).Encode(struct {
			Data any `json:"data"`
		}{data}); err != nil {
			t.Error("fixture response write failed")
		}
	}))
	defer server.Close()
	client := api.New(server.URL, config.Credential{}, server.Client())
	vault := PasswordVault{WorkspaceID: "personal", Client: client, Issuer: head.Issuer, AccountID: head.AccountID}
	for _, kind := range []string{"vault", "scope"} {
		t.Run(kind, func(t *testing.T) {
			call := func(ctx context.Context) error {
				if kind == "vault" {
					_, _, err := vault.current(ctx)
					return err
				}
				_, _, err := vault.readVaultScope(ctx, client, &keys, "personal", head.AccountID, "")
				return err
			}
			mode.Store(0)
			err := call(t.Context())
			var corrupt base64.CorruptInputError
			if !errors.As(err, &corrupt) || !errors.Is(err, ErrIntegrity) || !OwnsIntegrityFailure(err) {
				t.Fatal("API parser cause lost at manager")
			}
			if strings.Contains(err.Error(), canary) || err.Error() != "ENV encrypted state failed verification" {
				t.Fatal("verification error exposed private contents")
			}
			if OwnsIntegrityFailure(errors.Join(err, syscall.EIO)) {
				t.Fatal("integrity owner blessed independent I/O")
			}
			mode.Store(1)
			if err := call(t.Context()); err != nil {
				t.Fatal("fresh valid encrypted response did not recover")
			}
			mode.Store(2)
			err = call(t.Context())
			var status *api.APIError
			if !errors.As(err, &status) || status.Status != 503 || errors.Is(err, ErrIntegrity) || OwnsIntegrityFailure(err) {
				t.Fatal("HTTP failure became integrity")
			}
			mode.Store(3)
			err = call(t.Context())
			if !errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, ErrIntegrity) {
				t.Fatal("body read failure became integrity")
			}
			canceled, cancel := context.WithCancel(t.Context())
			cancel()
			err = call(canceled)
			if !errors.Is(err, context.Canceled) || errors.Is(err, ErrIntegrity) {
				t.Fatal("context cancellation became integrity")
			}
		})
	}
}

func TestENVIntegrityOwnershipDoesNotAuthorizeMixedFailure(t *testing.T) {
	state := api.VaultScopeState{Envelope: "!"}
	_, decodeErr := state.Decode()
	owned := wrapVaultIntegrityFailure(decodeErr)
	if !OwnsIntegrityFailure(owned) || !errors.Is(owned, ErrIntegrity) {
		t.Fatal("known memory decode not owned")
	}
	mixed := errors.Join(decodeErr, syscall.EIO)
	if result := wrapVaultIntegrityFailure(mixed); result != mixed || OwnsIntegrityFailure(result) || errors.Is(result, ErrIntegrity) {
		t.Fatal("mixed API/storage failure reclassified")
	}
	var nilOwner *vaultIntegrityFailure
	if OwnsIntegrityFailure(nilOwner) || nilOwner.Unwrap() != nil {
		t.Fatal("nil owner accepted")
	}
}
