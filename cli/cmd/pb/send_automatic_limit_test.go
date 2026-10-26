package main

import (
	"github.com/pinksaucepasta/paperboat/internal/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/filetransfer"
)

// PLAN 3b File transfer modes: Automatic sends at most 2,000,000,000 bytes in total (directories expanded, every
// file counted) and refuses a larger send before any network call, in one sentence.
func TestAutomaticSendBoundIsTheBatchTotal(t *testing.T) {
	sizes := []int64{api.AutomaticTransferLimit / 2, api.AutomaticTransferLimit / 2}
	sources := func(extra int64) []filetransfer.Source {
		out := make([]filetransfer.Source, len(sizes))
		for i, size := range sizes {
			out[i] = filetransfer.Source{Basename: "part" + string(rune('a'+i)), Size: size, Reader: strings.NewReader("")}
		}
		out[len(out)-1].Size += extra
		return out
	}
	if err := filetransfer.ValidateSources(sources(0), automaticTransferLimits); err != nil {
		t.Fatalf("exactly 2,000,000,000 bytes refused: %v", err)
	}
	err := filetransfer.ValidateSources(sources(1), automaticTransferLimits)
	var limit *filetransfer.LimitError
	if err == nil || !asLimit(err, &limit) {
		t.Fatalf("2,000,000,001 bytes accepted: %v", err)
	}
	if message := transferLimitMessage(limit); message != "This transfer is 2.0 GB. Automatic sends up to 2 GB. Use --unlimited to send directly or through your own relay. Nothing was sent." {
		t.Fatalf("message=%q", message)
	}
	big := &filetransfer.LimitError{Scope: filetransfer.LimitBatchBytes, Value: 3_400_000_000, Limit: api.AutomaticTransferLimit}
	if message := transferLimitMessage(big); !strings.HasPrefix(message, "This transfer is 3.4 GB. Automatic sends up to 2 GB.") {
		t.Fatalf("message=%q", message)
	}
}

func asLimit(err error, target **filetransfer.LimitError) bool {
	limit, ok := err.(*filetransfer.LimitError)
	*target = limit
	return ok
}

// Round 32: the daemon's stream lease was sized from the 50 MiB policy cap, so an Automatic send of
// 500 MB to 2 GB was cut by the sender's own daemon although the server and destination accepted it.
func TestTransferLeaseCoversTheDeclaredTotal(t *testing.T) {
	policy := api.FileTransferPolicy{MaxFileBytes: 50 << 20, MaxBatchBytes: 500 << 20}
	if got := transferLeaseBytes(policy, api.TransferDeclaration{Mode: "automatic", Bytes: 1_500_000_000}); got < 1_500_000_000 {
		t.Fatalf("lease %d bytes for a 1.5 GB declared send", got)
	}
	if got := transferLeaseBytes(policy, api.TransferDeclaration{}); got < 50<<20 || got > 51<<20 {
		t.Fatalf("lease %d bytes for an undeclared send, want the policy cap", got)
	}
}

// Round 33: pb send revalidated the files against the descriptor policy's 50 MiB caps after the server had
// signed a larger declared total, refusing every Automatic send over 50 MiB. A declared send is bounded by
// what was signed: its total for Automatic, nothing for Unlimited.
func TestDeclaredSendsAreBoundedByTheirDeclaration(t *testing.T) {
	policy := filetransfer.Limits{MaxFileBytes: 50 << 20, MaxBatchFiles: 10, MaxBatchBytes: 500 << 20}
	big := []filetransfer.Source{{Basename: "big.bin", Size: 1_500_000_000, Reader: strings.NewReader("")}}
	if err := filetransfer.ValidateSources(big, declaredTransferLimits(policy, api.TransferDeclaration{Mode: "automatic", Bytes: 1_500_000_000})); err != nil {
		t.Fatalf("a declared 1.5 GB Automatic send was refused: %v", err)
	}
	if err := filetransfer.ValidateSources(big, declaredTransferLimits(policy, api.TransferDeclaration{})); err == nil {
		t.Fatal("an undeclared send was not bounded by the policy")
	}
	huge := []filetransfer.Source{{Basename: "huge.bin", Size: 9_000_000_000, Reader: strings.NewReader("")}}
	if err := filetransfer.ValidateSources(huge, declaredTransferLimits(policy, api.TransferDeclaration{Mode: "unlimited", Bytes: 9_000_000_000})); err != nil {
		t.Fatalf("an Unlimited send was bounded: %v", err)
	}
}

// --unlimited wins; otherwise the account's preference; Automatic when it cannot be read.
func TestSendTransferModeFollowsFlagThenAccountPreference(t *testing.T) {
	mode := "unlimited"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/me/preferences" || mode == "" {
			http.NotFound(w, r)
			return
		}
		writeAPIData(t, w, map[string]string{"file_transfer_mode": mode})
	}))
	defer srv.Close()
	client := api.New(srv.URL, config.Credential{AccessToken: "token"}, srv.Client())
	command := sendCommand()
	if got := sendTransferMode(t.Context(), command, client); got != "unlimited" {
		t.Fatalf("preference unlimited gave %q", got)
	}
	mode = ""
	if got := sendTransferMode(t.Context(), command, client); got != "automatic" {
		t.Fatalf("unreadable preference gave %q", got)
	}
	_ = command.Flags().Set("unlimited", "true")
	if got := sendTransferMode(t.Context(), command, client); got != "unlimited" {
		t.Fatalf("--unlimited gave %q", got)
	}
}
