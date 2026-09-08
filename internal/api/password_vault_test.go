package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestPasswordVaultHTTPBoundary(t *testing.T) {
	fixture, err := os.ReadFile("../../testdata/contracts/environment-e2ee-v1/password-vault.json")
	if err != nil {
		t.Fatal(err)
	}
	var state PasswordVaultState
	if err := json.Unmarshal(fixture, &state); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(state.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	cacheControl := "no-store"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/environment-vault" {
			t.Error("wrong vault route")
		}
		if r.Method == http.MethodPut {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			var fields map[string]string
			if json.Unmarshal(body, &fields) != nil || len(fields) != 1 || fields["envelope"] != state.Envelope || bytes.Contains(body, []byte("public-test-vector-password")) {
				t.Error("PUT leaked data or changed ciphertext")
			}
		}
		w.Header().Set("Cache-Control", cacheControl)
		writeData(w, http.StatusOK, state)
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{}, server.Client())
	if _, err := client.GetPasswordVault(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PutPasswordVault(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	cacheControl = "public"
	if _, err := client.GetPasswordVault(context.Background()); err == nil {
		t.Fatal("cacheable vault accepted")
	}
	cacheControl = "no-store"
	state.DocumentID = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := client.GetPasswordVault(context.Background()); err == nil {
		t.Fatal("incorrect envelope digest accepted")
	}
}

func TestPasswordVaultResetHTTPBoundary(t *testing.T) {
	fixture, err := os.ReadFile("../../testdata/contracts/environment-e2ee-v1/password-vault.json")
	if err != nil {
		t.Fatal(err)
	}
	var state PasswordVaultState
	if err := json.Unmarshal(fixture, &state); err != nil {
		t.Fatal(err)
	}
	input := VaultReset{OperationID: "reset_contract_operation", ExpectedVaultDocumentID: state.DocumentID, VaultEnvelope: state.Envelope, ScopeEnvelopes: []string{}, ConfirmTotalLoss: true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/environment-vault/reset" {
			t.Error("reset must target the password-vault route")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var got VaultReset
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil || got.OperationID != input.OperationID || got.VaultEnvelope != input.VaultEnvelope || got.ExpectedVaultDocumentID != input.ExpectedVaultDocumentID || !got.ConfirmTotalLoss {
			t.Error("reset transaction changed")
		}
		w.Header().Set("Cache-Control", "no-store")
		writeData(w, http.StatusOK, state)
	}))
	defer server.Close()
	if _, err := New(server.URL, config.Credential{}, server.Client()).ResetVault(context.Background(), input); err != nil {
		t.Fatal(err)
	}
}
