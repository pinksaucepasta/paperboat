package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
	"github.com/spf13/cobra"
)

// doctorClockTimeout bounds each of the (at most two) unauthenticated server requests that read the
// server's clock; both and the pause between them stay inside the probe's 10 s deadline.
var doctorClockTimeout = 4 * time.Second

const doctorClockRetryPause = 250 * time.Millisecond

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
		check.Status, check.Summary = doctorpkg.StatusWarning, clockUnreadableSummary(err)
		check.Recovery = "Check the connection to Paperboat and run `pb doctor` again."
		return check
	}
	if offset != 0 {
		check.Status = doctorpkg.StatusWarning
		check.Summary = "This machine's clock is off by " + describeClockOffset(offset) + "; Paperboat corrects for it."
		check.Recovery = "Turn on automatic time (NTP) for this machine. A clock set back by minutes can keep other machines from connecting to this one until it catches up."
	}
	return check
}

// clockUnreadableSummary names why the server's clock could not be compared: a slow network, a
// reply without a time, or a time this machine cannot read are different problems.
func clockUnreadableSummary(err error) string {
	switch {
	case errors.Is(err, serverclock.ErrNoDate):
		return "The Paperboat server's reply carries no time, so its clock could not be compared."
	case errors.Is(err, serverclock.ErrBadDate):
		return "The Paperboat server's reply has a time that could not be read, so its clock could not be compared."
	case errors.Is(err, serverclock.ErrSlowRoundTrip):
		return "The connection to the Paperboat server was too slow to compare clocks accurately."
	case isTimeout(err):
		return "The Paperboat server did not answer in time, so its clock could not be read."
	}
	return "The Paperboat server could not be reached to read its clock."
}

func isTimeout(err error) bool {
	var network net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) && network.Timeout()
}

// measureServerClock reads how far the server's clock is ahead from this request's own Date header.
// A failed request is tried once more (a cold first connection often needs it); a reply without a
// usable Date is not, it would fail the same way.
func measureServerClock(ctx context.Context, serverURL string) (time.Duration, error) {
	normalized, err := config.NormalizeServerURL(serverURL)
	if err != nil {
		return 0, err
	}
	offset, err := requestServerClock(ctx, normalized)
	if err == nil || errors.Is(err, serverclock.ErrNoDate) || errors.Is(err, serverclock.ErrBadDate) || errors.Is(err, serverclock.ErrSlowRoundTrip) {
		return offset, err
	}
	select {
	case <-ctx.Done():
		return 0, err
	case <-time.After(doctorClockRetryPause):
	}
	return requestServerClock(ctx, normalized)
}

func requestServerClock(ctx context.Context, normalized string) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, doctorClockTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, normalized+"/v1/server-version", nil)
	if err != nil {
		return 0, err
	}
	client := &http.Client{Transport: errorreport.Transport(nil, normalized), Timeout: doctorClockTimeout}
	sent := time.Now()
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	received := time.Now()
	_ = response.Body.Close()
	return serverclock.Measure(response.Header.Get("Date"), sent, received)
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
