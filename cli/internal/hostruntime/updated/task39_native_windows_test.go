//go:build windows

package updated

import (
	"context"
	"errors"
	"flag"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
)

var task39WindowsUpdateSocket = flag.String("task39-windows-update-socket", "", "Task39 updater control pipe")
var task39WindowsUpdateWantError = flag.Bool("task39-windows-update-want-error", false, "expect candidate rejection")
var task39WindowsUpdateVersion = flag.String("task39-windows-update-version", "", "required activated candidate version")

func TestTask39WindowsUpdateControl(t *testing.T) {
	if *task39WindowsUpdateSocket == "" {
		t.Skip("native Windows update fixture")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		t.Fatal("native update requires the enrolled user's token")
	}
	instance, err := service.WindowsUserInstance(user.User.Sid.String())
	if err != nil || *task39WindowsUpdateSocket != `\\.\pipe\PaperboatUpdatedControl-`+instance {
		t.Fatal("native update must run as the owner of the exact updater pipe")
	}
	client, err := NewClient(*task39WindowsUpdateSocket, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	probe := func() (ControlResponse, error) {
		probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
		defer probeCancel()
		return client.Status(probeCtx)
	}
	before, err := probe()
	if err != nil || before.Version == "" || before.Pending {
		t.Fatalf("updater must be usable before request: response=%+v error=%v", before, err)
	}
	response, err := client.Update(ctx)
	if *task39WindowsUpdateWantError {
		var rejected *ControlError
		if !errors.As(err, &rejected) || rejected.Code != "check_failed" {
			t.Fatalf("expected candidate rejection from updater, got response=%+v error=%v", response, err)
		}
		after, statusErr := probe()
		if statusErr != nil || after.Version != before.Version || after.Pending {
			t.Fatalf("rejected candidate changed usable installation: response=%+v error=%v", after, statusErr)
		}
		t.Logf("candidate rejected: %v", err)
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if *task39WindowsUpdateVersion == "" || response.Version != *task39WindowsUpdateVersion || !response.Pending && !response.Updated {
		t.Fatalf("candidate was not accepted for activation: %+v", response)
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		status, statusErr := probe()
		if statusErr == nil && status.Version == *task39WindowsUpdateVersion && status.Updated && !status.Pending {
			t.Logf("activated update: %+v", status)
			return
		}
		if statusErr == nil && status.ActivationFailure != "" {
			t.Fatalf("activation failed: %+v", status)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("activation did not complete: response=%+v error=%v", status, statusErr)
		case <-ticker.C:
		}
	}
}
