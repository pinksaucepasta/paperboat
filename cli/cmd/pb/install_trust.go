package main

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/machineguard"
)

// Read the installed guard's authenticated status after installation or pairing,
// including when a ready guard was reused. Browser trust is separate from service
// readiness and must not turn a committed installation into a failed command.
func installedTrustWarnings(ctx context.Context) []string {
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	client, err := machineguard.Connect(probe, machineguard.DefaultSocket)
	if err != nil {
		return nil
	}
	defer client.Close()
	status, err := client.Status(probe)
	if err != nil || !status.Ready || status.TrustReady {
		return nil
	}
	return []string{"Paperboat is installed and its local service is ready; browser HTTPS trust is pending. Recovery: retry `pb install` and approve the operating system certificate trust request."}
}
