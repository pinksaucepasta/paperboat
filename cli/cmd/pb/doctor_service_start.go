package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/diagnostics"
	doctorpkg "github.com/pinksaucepasta/paperboat/cli/internal/doctor"
	"github.com/pinksaucepasta/paperboat/cli/internal/localdaemon"
)

const serviceStartTimeoutWindow = 24 * time.Hour

// probeDoctorServiceStart reports Windows starts of the local daemon service that
// did not answer in time. The start is not treated as a failure (the service may
// still come up), but a pb command waits silently while it does.
func probeDoctorServiceStart(ctx context.Context) doctorpkg.Check {
	check := doctorpkg.Check{Category: "local", Code: "local_daemon_service_start", Status: doctorpkg.StatusPass, Summary: "The local Paperboat service starts promptly."}
	recorder := diagnostics.FromContext(ctx)
	if recorder == nil {
		return check
	}
	tail, err := recorder.ReadDiskTail(ctx, 1<<20)
	if err != nil {
		return check
	}
	if stalled, last := countServiceStartTimeouts(tail, time.Now().Add(-serviceStartTimeoutWindow)); stalled > 0 {
		check.Status = doctorpkg.StatusWarning
		check.Summary = fmt.Sprintf("Windows reported %d slow start(s) of the local Paperboat service in the last 24 hours (last at %s); commands wait while it starts.", stalled, last.Local().Format("15:04"))
		check.Recovery = "If this continues, restart the computer or reinstall Paperboat, then run pb doctor again."
	}
	return check
}

func countServiceStartTimeouts(events []byte, since time.Time) (int, time.Time) {
	count, last := 0, time.Time{}
	scanner := bufio.NewScanner(bytes.NewReader(events))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var event diagnostics.Event
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.At.Before(since) || event.Code != localdaemon.ServiceStartTimeoutCode {
			continue
		}
		count++
		if event.At.After(last) {
			last = event.At
		}
	}
	return count, last
}
