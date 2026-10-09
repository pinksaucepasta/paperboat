//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
)

func TestUpdaterControlFailureUsesFiniteJSONAndOmitsRemoteMessage(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "updater.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer connection.Close()
		var request updated.ControlRequest
		if err := json.NewDecoder(connection).Decode(&request); err != nil {
			serverDone <- err
			return
		}
		if request.Operation != "download" {
			serverDone <- fmt.Errorf("operation = %q, want download", request.Operation)
			return
		}
		serverDone <- json.NewEncoder(connection).Encode(updated.ControlResponse{
			Schema: updated.ControlProtocolV1, Status: "error", ErrorCode: "check_failed",
			ErrorMessage: `open C:\Users\private\Paperboat\state: Access is denied; token=remote-secret`,
		})
	}()

	client, err := updated.NewClient(listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, controlErr := client.Download(context.Background())
	if controlErr == nil {
		t.Fatal("download unexpectedly succeeded")
	}
	if err := <-serverDone; err != nil {
		t.Fatal("control fixture failed")
	}

	var output bytes.Buffer
	joinedFailure := errors.Join(fmt.Errorf("download signed update: %w", controlErr), errors.New("socket close failed"))
	if err := writeCLIJSONError(&output, joinedFailure); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		OK    bool `json:"ok"`
		Error *struct {
			Code             string `json:"code"`
			Stage            string `json:"stage"`
			Category         string `json:"category"`
			Message          string `json:"message"`
			Retryable        bool   `json:"retryable"`
			StateChanged     any    `json:"state_changed"`
			OutcomeUncertain bool   `json:"outcome_uncertain"`
			Recovery         string `json:"recovery"`
		} `json:"error"`
	}
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.OK || envelope.Error == nil {
		t.Fatal("updater failure did not produce an error envelope")
	}
	failure := envelope.Error
	if failure.Code != "updater_check_failed" || failure.Stage != "update_download" || failure.Category != "unavailable_retryable" || !failure.Retryable {
		t.Fatalf("updater classification code=%q stage=%q category=%q retryable=%t", failure.Code, failure.Stage, failure.Category, failure.Retryable)
	}
	if failure.Message != "The local updater could not verify or download the signed update." || !strings.Contains(failure.Recovery, "pb update status") {
		t.Fatal("updater failure is missing actionable recovery")
	}
	for _, private := range []string{`C:\Users\private`, "Access is denied", "remote-secret", "socket close failed"} {
		if strings.Contains(output.String(), private) {
			t.Fatal("remote updater detail escaped CLI JSON")
		}
	}

	schemaBytes, err := os.ReadFile(filepath.Join("..", "..", "testdata", "contracts", "schemas", "cli", "output.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Error struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"error"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schemaBytes, &schema); err != nil {
		t.Fatal(err)
	}
	var stage struct {
		Enum []string `json:"enum"`
	}
	stageJSON, ok := schema.Properties.Error.Properties["stage"]
	if !ok || json.Unmarshal(stageJSON, &stage) != nil || !slices.Equal(stage.Enum, []string{"update_check", "update_download", "update_install", "control_request"}) {
		t.Fatalf("CLI JSON stage schema is absent or incomplete (present=%t)", ok)
	}
}
