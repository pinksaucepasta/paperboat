package filetransfer

import (
	"errors"
	"slices"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

var ErrNoActiveWriter = errors.New("no_active_writer")

// WriterRecipient selects from PTY-owner state, so carrier and hostd
// replacement cannot erase or fork the current file recipient.
func WriterRecipient(writers []session.ActiveWriter, machineID string) (string, error) {
	clients := make(map[string]time.Time)
	for _, writer := range writers {
		if machineID != "" && writer.MachineID != machineID {
			continue
		}
		if writer.LastInput.After(clients[writer.ClientID]) {
			clients[writer.ClientID] = writer.LastInput
		} else if _, ok := clients[writer.ClientID]; !ok {
			clients[writer.ClientID] = time.Time{}
		}
	}
	var selected string
	var latest time.Time
	for client, at := range clients {
		if !at.IsZero() && (selected == "" || at.After(latest) || at.Equal(latest) && client < selected) {
			selected, latest = client, at
		}
	}
	if selected != "" {
		return selected, nil
	}
	if len(clients) == 1 {
		for client := range clients {
			return client, nil
		}
	}
	return "", ErrNoActiveWriter
}

func EligibleWriterMachines(writers []session.ActiveWriter) []string {
	seen := make(map[string]struct{})
	for _, writer := range writers {
		if writer.MachineID != "" {
			seen[writer.MachineID] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for machineID := range seen {
		result = append(result, machineID)
	}
	slices.Sort(result)
	return result
}
