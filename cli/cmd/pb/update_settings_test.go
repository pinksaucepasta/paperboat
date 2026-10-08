//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/spf13/cobra"
)

type updateSettingsControlFixture struct {
	listener *net.UnixListener
	done     chan struct{}
	mu       sync.Mutex
	requests []updated.ControlRequest
	settings autoupdate.Preferences
}

func startUpdateSettingsControlFixture(t *testing.T) *updateSettingsControlFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "updater.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &updateSettingsControlFixture{
		listener: listener,
		done:     make(chan struct{}),
		settings: autoupdate.DefaultPreferences(true),
	}
	go fixture.serve()
	t.Cleanup(func() {
		_ = fixture.listener.Close()
		select {
		case <-fixture.done:
		case <-time.After(time.Second):
			t.Error("local updater settings fixture did not stop")
		}
	})
	return fixture
}

func (f *updateSettingsControlFixture) serve() {
	defer close(f.done)
	for {
		connection, err := f.listener.AcceptUnix()
		if err != nil {
			return
		}
		f.handle(connection)
	}
}

func (f *updateSettingsControlFixture) handle(connection *net.UnixConn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	var request updated.ControlRequest
	decoder := json.NewDecoder(io.LimitReader(connection, 4<<10))
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&request)
	if decodeErr == nil {
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			decodeErr = errors.New("updater request contained trailing data")
		}
	}
	if decodeErr == nil {
		f.mu.Lock()
		f.requests = append(f.requests, request)
		if request.Operation == "settings" && request.Settings != nil {
			f.settings = *request.Settings
		}
		settings := f.settings
		f.mu.Unlock()

		response := updated.ControlResponse{
			Schema:   updated.ControlProtocolV1,
			Status:   "ok",
			Settings: &settings,
		}
		if settings.Enabled {
			response.NextMaintenanceAt = time.Now().In(time.Local).Add(24 * time.Hour)
		}
		_ = json.NewEncoder(connection).Encode(response)
		_ = connection.CloseWrite()
		return
	}
	response := updated.ControlResponse{
		Schema:       updated.ControlProtocolV1,
		Status:       "error",
		ErrorCode:    "invalid_request",
		ErrorMessage: "invalid local test request",
	}
	_ = json.NewEncoder(connection).Encode(response)
	_ = connection.CloseWrite()
}

func (f *updateSettingsControlFixture) recordedRequests() []updated.ControlRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	requests := append([]updated.ControlRequest(nil), f.requests...)
	for i := range requests {
		if requests[i].Settings != nil {
			settings := *requests[i].Settings
			requests[i].Settings = &settings
		}
	}
	return requests
}

func runUpdateSettingsCommand(t *testing.T, fixture *updateSettingsControlFixture, args ...string) (string, string, error) {
	t.Helper()
	previousSocket := updateControlSocketForCommand
	updateControlSocketForCommand = func() (string, error) {
		return fixture.listener.Addr().String(), nil
	}
	t.Cleanup(func() { updateControlSocketForCommand = previousSocket })

	root := &cobra.Command{Use: "pb", SilenceErrors: true, SilenceUsage: true}
	update := &cobra.Command{Use: "update"}
	update.AddCommand(updateSettingsCommand())
	root.AddCommand(update)
	root.SetArgs(append([]string{"update", "settings"}, args...))
	root.SetIn(strings.NewReader(""))
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	err := root.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

func settingsControlRequest(value *autoupdate.Preferences) updated.ControlRequest {
	return updated.ControlRequest{
		Schema:    updated.ControlProtocolV1,
		Operation: "settings",
		Settings:  value,
	}
}

func requireSettingsRequests(t *testing.T, fixture *updateSettingsControlFixture, want ...updated.ControlRequest) {
	t.Helper()
	if got := fixture.recordedRequests(); !reflect.DeepEqual(got, want) {
		t.Fatalf("updater requests = %#v, want %#v", got, want)
	}
}

func TestUpdateSettingsReadsByDefaultAndWritesOnlyChangedValues(t *testing.T) {
	initial := autoupdate.DefaultPreferences(true)
	get := settingsControlRequest(nil)

	t.Run("no flags only reads and shows local schedule", func(t *testing.T) {
		fixture := startUpdateSettingsControlFixture(t)
		stdout, stderr, err := runUpdateSettingsCommand(t, fixture)
		if err != nil {
			t.Fatal(err)
		}
		if stderr != "" {
			t.Fatalf("stderr = %q", stderr)
		}
		zone := updateSettingsCommandResult(initial, time.Now()).TimeZone
		for _, text := range []string{
			"Automatic updates: On",
			"Daily time: 04:00 " + zone,
			"Next maintenance:",
			"downloads and installs updates",
		} {
			if !strings.Contains(stdout, text) {
				t.Fatalf("output %q does not contain %q", stdout, text)
			}
		}
		requireSettingsRequests(t, fixture, get)
	})

	t.Run("changing only time preserves automatic updates", func(t *testing.T) {
		fixture := startUpdateSettingsControlFixture(t)
		stdout, _, err := runUpdateSettingsCommand(t, fixture, "--time", "22:15")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout, "Daily time: 22:15 ") {
			t.Fatalf("changed local time was not shown: %q", stdout)
		}
		want := autoupdate.Preferences{Schema: autoupdate.PreferencesSchema, Enabled: true, LocalTime: "22:15"}
		requireSettingsRequests(t, fixture, get, settingsControlRequest(&want))
	})

	t.Run("opt-out preserves the configured time", func(t *testing.T) {
		fixture := startUpdateSettingsControlFixture(t)
		stdout, _, err := runUpdateSettingsCommand(t, fixture, "--auto=false")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout, "Automatic updates: Off") ||
			!strings.Contains(stdout, "Availability checks continue") ||
			!strings.Contains(stdout, "Daily time: 04:00 ") {
			t.Fatalf("opt-out output did not explain preserved behavior: %q", stdout)
		}
		want := autoupdate.Preferences{Schema: autoupdate.PreferencesSchema, Enabled: false, LocalTime: "04:00"}
		requireSettingsRequests(t, fixture, get, settingsControlRequest(&want))
	})

	t.Run("invalid time is rejected before contacting the updater", func(t *testing.T) {
		fixture := startUpdateSettingsControlFixture(t)
		_, _, err := runUpdateSettingsCommand(t, fixture, "--time", "25:60")
		if err == nil || !strings.Contains(err.Error(), "HH:MM") {
			t.Fatalf("invalid-time error = %v", err)
		}
		requireSettingsRequests(t, fixture)
	})

	t.Run("JSON mode is clean and read-only without setting flags", func(t *testing.T) {
		fixture := startUpdateSettingsControlFixture(t)
		stdout, stderr, err := runUpdateSettingsCommand(t, fixture, "--json")
		if err != nil {
			t.Fatal(err)
		}
		if stderr != "" || strings.Contains(stdout, "Automatic updates:") {
			t.Fatalf("JSON mode mixed presentation into output: stdout=%q stderr=%q", stdout, stderr)
		}
		var result struct {
			SchemaVersion string `json:"schema_version"`
			OK            bool   `json:"ok"`
			Data          struct {
				AutomaticUpdates bool   `json:"automatic_updates"`
				LocalTime        string `json:"local_time"`
				TimeZone         string `json:"time_zone"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdout), &result); err != nil {
			t.Fatalf("JSON output %q: %v", stdout, err)
		}
		if result.SchemaVersion != "1.0" || !result.OK || !result.Data.AutomaticUpdates ||
			result.Data.LocalTime != "04:00" || result.Data.TimeZone == "" {
			t.Fatalf("unexpected JSON settings: %+v", result)
		}
		requireSettingsRequests(t, fixture, get)
	})
}
