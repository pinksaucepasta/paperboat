package main

import (
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
	if message := transferLimitMessage(limit); message != "This transfer is 2.0 GB. Automatic sends up to 2 GB. Nothing was sent." {
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
