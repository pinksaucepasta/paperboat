package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
	helperconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/enrollment"
)

// credentialRefusalWindow and credentialRefusalThreshold decide when refusals are a pattern, not one blip.
const (
	credentialRefusalWindow    = time.Hour
	credentialRefusalThreshold = 3
)

// probeDoctorCredentials reads this machine's recent diagnostics events: a steady stream of machine-control
// requests refused with 401 meant the server no longer accepted this machine's credentials, while doctor said
// healthy (lanep-r26b, ~28 an hour all day, 2026-10-25).
func probeDoctorCredentials(ctx context.Context) doctorpkg.Check {
	check := doctorpkg.Check{Category: "local", Code: "machine_credentials", Status: doctorpkg.StatusPass, Summary: "The Paperboat server accepts this machine's credentials."}
	if renewalRefusedHere() {
		check.Status = doctorpkg.StatusFail
		check.Summary = "The Paperboat server no longer accepts this machine: it was removed or its credentials were revoked. Paperboat has stopped retrying."
		check.Recovery = "Run `pb pair` with a new enrollment token to add this machine again."
		return check
	}
	recorder := diagnostics.FromContext(ctx)
	if recorder == nil {
		return check
	}
	tail, err := recorder.ReadDiskTail(ctx, 1<<20)
	if err != nil {
		return check
	}
	refused := countCredentialRefusals(tail, time.Now().Add(-credentialRefusalWindow))
	if refused >= credentialRefusalThreshold {
		check.Status = doctorpkg.StatusWarning
		check.Summary = fmt.Sprintf("The Paperboat server refused this machine's credentials %d times in the last hour.", refused)
		check.Recovery = "Paperboat renews them on its own; if this continues, run `pb login --reauth`, then `pb doctor` again."
	}
	return check
}

// countCredentialRefusals counts machine-control requests refused with 401 since the given time.
func countCredentialRefusals(events []byte, since time.Time) int {
	count := 0
	scanner := bufio.NewScanner(bytes.NewReader(events))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var event diagnostics.Event
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.At.Before(since) || event.Code != "control_request_failed" {
			continue
		}
		if event.Fields["operation"] == "machine_control" && (event.Fields["http_status"] == "401" || event.Fields["cause"] == "permission_denied") {
			count++
		}
	}
	return count
}

// renewalRefusedHere reads whether the daemon recorded that the server refused this machine's identity renewal (401).
var renewalRefusedHere = func() bool {
	root := strings.TrimSpace(os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT"))
	if root == "" {
		var err error
		if root, err = helperconfig.DefaultStateRoot(os.Getenv); err != nil {
			return false
		}
	}
	return enrollment.RenewalRefused(root, time.Now().UTC())
}
