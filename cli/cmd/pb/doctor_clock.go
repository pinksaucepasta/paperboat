package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/serverclock"
	"github.com/spf13/cobra"
)

// doctorClockTimeout bounds the one unauthenticated server request that reads the server's clock.
const doctorClockTimeout = 5 * time.Second

// probeDoctorClock compares this machine's clock with the Paperboat server's. Paperboat corrects for
// an offset itself, but a clock that is far off also breaks TLS and other software, so it is named.
func probeDoctorClock(ctx context.Context, command *cobra.Command) doctorpkg.Check {
	check := doctorpkg.Check{Category: "local", Code: "clock", Status: doctorpkg.StatusPass, Summary: "This machine's clock matches the Paperboat server's."}
	serverURL := ""
	if cfg, err := config.Load(configPathFlag(command)); err == nil {
		serverURL = cfg.ServerURL
	}
	if override, _ := command.Flags().GetString("server"); strings.TrimSpace(override) != "" {
		serverURL = override
	}
	offset, err := measureServerClock(ctx, serverURL)
	if err != nil {
		check.Status, check.Summary = doctorpkg.StatusWarning, "The server's clock could not be read."
		check.Recovery = "Check the connection to Paperboat and run `pb doctor` again."
		return check
	}
	if offset != 0 {
		check.Status = doctorpkg.StatusWarning
		check.Summary = "This machine's clock is off by " + describeClockOffset(offset) + "; Paperboat corrects for it."
		check.Recovery = "Turn on automatic time (NTP) for this machine."
	}
	return check
}

func measureServerClock(ctx context.Context, serverURL string) (time.Duration, error) {
	normalized, err := config.NormalizeServerURL(serverURL)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, doctorClockTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, normalized+"/v1/server-version", nil)
	if err != nil {
		return 0, err
	}
	client := &http.Client{Transport: errorreport.Transport(nil, normalized), Timeout: doctorClockTimeout}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	_ = response.Body.Close()
	if response.Header.Get("Date") == "" {
		return 0, fmt.Errorf("server response has no Date")
	}
	return serverclock.Offset(), nil
}

// describeClockOffset says how far and which way: "12 minutes ahead".
func describeClockOffset(offset time.Duration) string {
	direction := "behind"
	if offset < 0 {
		direction, offset = "ahead", -offset
	}
	minutes := int(offset.Round(time.Minute) / time.Minute)
	if minutes >= 120 {
		return fmt.Sprintf("%d hours %s", minutes/60, direction)
	}
	unit := "minutes"
	if minutes == 1 {
		unit = "minute"
	}
	return fmt.Sprintf("%d %s %s", minutes, unit, direction)
}
