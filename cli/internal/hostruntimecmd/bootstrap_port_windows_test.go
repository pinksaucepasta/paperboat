//go:build windows

package hostruntimecmd

import (
	"context"
	"errors"
	"flag"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/windowsopenssh"
)

var task39NativePortCheck = flag.Bool("task39-native-port-check", false, "check the current enrolled Windows account's existing managed SSH listener")

func TestWindowsBootstrapPortRetryRequiresExactInstalledOwner(t *testing.T) {
	const instance = "u0123456789abcdef01234567"
	config := windowsOpenSSHBootstrapConfig(windowsopenssh.DefaultConfig(nil), instance)
	health := windowsopenssh.ServiceHealth{
		Service: windowsopenssh.ServiceRecord{Name: config.ServiceName, Exists: true, State: "Running", ProcessID: 501, PathName: `"` + config.ServiceExecutable + `" daemon __windows-sshd-service --instance ` + instance},
		Listeners: []windowsopenssh.ListenerRecord{
			{Address: "127.0.0.1", Port: config.Port, ProcessID: 502, ParentProcessID: 501, ExecutablePath: filepath.Join(config.InstallRoot, "sshd.exe")},
			{Address: "::1", Port: config.Port, ProcessID: 502, ParentProcessID: 501, ExecutablePath: filepath.Join(config.InstallRoot, "sshd.exe")},
		},
	}
	busy := errors.New("port occupied")
	for _, test := range []struct {
		name    string
		mutate  func(*windowsopenssh.ServiceHealth)
		allowed bool
	}{
		{name: "own listener", allowed: true},
		{name: "foreign service", mutate: func(h *windowsopenssh.ServiceHealth) { h.Service.Name += "other" }},
		{name: "foreign instance", mutate: func(h *windowsopenssh.ServiceHealth) { h.Service.PathName += "other" }},
		{name: "foreign wrapper", mutate: func(h *windowsopenssh.ServiceHealth) {
			h.Service.PathName = `"` + config.ServiceExecutable + `.other" daemon __windows-sshd-service --instance ` + instance
		}},
		{name: "foreign child", mutate: func(h *windowsopenssh.ServiceHealth) { h.Listeners[0].ParentProcessID++ }},
		{name: "foreign executable", mutate: func(h *windowsopenssh.ServiceHealth) { h.Listeners[0].ExecutablePath += ".other" }},
		{name: "wildcard", mutate: func(h *windowsopenssh.ServiceHealth) { h.Listeners[0].Address = "0.0.0.0" }},
		{name: "missing family", mutate: func(h *windowsopenssh.ServiceHealth) { h.Listeners = h.Listeners[:1] }},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := health
			value.Listeners = append([]windowsopenssh.ListenerRecord(nil), health.Listeners...)
			if test.mutate != nil {
				test.mutate(&value)
			}
			err := checkWindowsBootstrapSSHPortWith(context.Background(), config, func(uint16) error { return busy }, func(_ context.Context, config windowsopenssh.Config, result windowsopenssh.Result) (windowsopenssh.ServiceHealth, error) {
				return value, windowsopenssh.ValidateLoopbackHealth(value, config, result)
			})
			if (err == nil) != test.allowed {
				t.Fatalf("allowed=%t error=%v", test.allowed, err)
			}
			if !test.allowed && !errors.Is(err, busy) {
				t.Fatalf("lost port conflict: %v", err)
			}
		})
	}
	if err := checkWindowsBootstrapSSHPortWith(context.Background(), config, func(uint16) error { return nil }, func(context.Context, windowsopenssh.Config, windowsopenssh.Result) (windowsopenssh.ServiceHealth, error) {
		t.Fatal("unused port must not require an installed service")
		return windowsopenssh.ServiceHealth{}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsBootstrapPortRetryOnNativeInstance(t *testing.T) {
	if !*task39NativePortCheck {
		t.Skip("requires an installed task-owned Windows account")
	}
	sid, err := currentWindowsSID()
	if err != nil || sid == "S-1-5-18" {
		t.Fatalf("requires enrolled user token: %v", err)
	}
	instance, err := service.WindowsUserInstance(sid)
	if err != nil {
		t.Fatal(err)
	}
	config := windowsOpenSSHBootstrapConfig(windowsopenssh.DefaultConfig(nil), instance)
	if err := windowsopenssh.CheckPortAvailable(config.Port); err == nil {
		t.Fatal("native retry check requires the installed listener")
	}
	if err := checkWindowsBootstrapSSHPort(context.Background(), config); err != nil {
		t.Fatal(err)
	}
}
