package filetransfer

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"testing"
	"time"
)

func TestWriterRegistryRoutesToExactMachine(t *testing.T) {
	writers := []session.ActiveWriter{{ClientID: "cli_a", MachineID: "machine_a"}}
	if got, err := WriterRecipient(writers, "machine_a"); err != nil || got != "cli_a" {
		t.Fatalf("sole=%q err=%v", got, err)
	}
	writers = append(writers, session.ActiveWriter{ClientID: "cli_b", MachineID: "machine_b"})
	if _, err := WriterRecipient(writers, "machine_missing"); !errors.Is(err, ErrNoActiveWriter) {
		t.Fatalf("ambiguous err=%v", err)
	}
	now := time.Now()
	writers[0].LastInput = now
	writers[1].LastInput = now.Add(time.Second)
	if got, err := WriterRecipient(writers, "machine_b"); err != nil || got != "cli_b" {
		t.Fatalf("last=%q err=%v", got, err)
	}
	writers = writers[:1]
	if got, err := WriterRecipient(writers, "machine_a"); err != nil || got != "cli_a" {
		t.Fatalf("fallback=%q err=%v", got, err)
	}
	if got := EligibleWriterMachines(writers); len(got) != 1 || got[0] != "machine_a" {
		t.Fatalf("eligible=%v", got)
	}
}
