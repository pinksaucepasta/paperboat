package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	helperconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machinecontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	"github.com/spf13/cobra"
)

var (
	// These sentinels let callers and tests branch on a stable failure class
	// without depending on the human-readable detail appended to the error.
	ErrPreviewOriginUnavailable       = errors.New("preview origin unavailable")
	ErrPreviewInvalidTarget           = errors.New("invalid preview target")
	ErrPreviewInvalidDuration         = errors.New("invalid preview duration")
	ErrPreviewCarrierUnavailable      = errors.New("preview carrier unavailable")
	ErrPreviewMachineNotConfigured    = errors.New("preview machine is not configured")
	ErrPreviewOwnerSessionUnavailable = errors.New("preview owner session unavailable")
)

// previewCarrierFactory is the narrow runtime composition seam. TRK-10 owns
// the foreground session lifecycle and TRK-14 owns carrier implementations;
// the CLI must not invent a fake ready state when the runtime has not attached
// a production carrier yet.
type previewCarrierFactory func(context.Context, preview.LeaseTarget, string, string) (preview.Carrier, error)

var newPreviewCarrier previewCarrierFactory = newProductionPreviewCarrier

var previewClientForCommand = func(command *cobra.Command) (*api.Client, error) {
	return backendClient(actionContext(command, nil))
}

var previewMachineID = configuredMachineID

var previewOwnerSessionClientForCommand = newProductionPreviewOwnerSessionClient

type previewFailureError struct {
	message string
	causes  []error
}

func (e previewFailureError) Error() string { return e.message }

func (e previewFailureError) Unwrap() []error { return e.causes }

func previewFailure(message string, causes ...error) error {
	filtered := make([]error, 0, len(causes))
	for _, cause := range causes {
		if cause != nil {
			filtered = append(filtered, cause)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return previewFailureError{message: message, causes: filtered}
}

// onlyCause reports whether every leaf of err is target, so a refusal never
// hides an independent cleanup failure joined beside it.
func onlyCause(err, target error) bool {
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		children := wrapped.Unwrap()
		found := false
		for _, child := range children {
			if child == nil {
				continue
			}
			if !onlyCause(child, target) {
				return false
			}
			found = true
		}
		return found
	case interface{ Unwrap() error }:
		return wrapped.Unwrap() != nil && onlyCause(wrapped.Unwrap(), target)
	}
	return errors.Is(err, target)
}

func closePreviewCarrier(ctx context.Context, carrier preview.Carrier, primary error) error {
	if carrier == nil {
		return primary
	}
	closeErr := carrier.Close(context.WithoutCancel(ctx))
	if closeErr == nil {
		return primary
	}
	if primary == nil {
		return previewFailure("preview carrier cleanup failed", closeErr)
	}
	return previewFailure("preview setup failed and carrier cleanup was incomplete", primary, closeErr)
}

const (
	previewBackgroundDefaultTTL = 30 * time.Minute
	previewMaximumTTL           = 24 * time.Hour
)

func newProductionPreviewCarrier(ctx context.Context, target preview.LeaseTarget, machineID, ownerSessionID string) (preview.Carrier, error) {
	if ctx == nil {
		return nil, previewFailure(ErrPreviewMachineNotConfigured.Error(), ErrPreviewMachineNotConfigured)
	}
	// The stable host runtime owns the authenticated data carrier. A CLI
	// process only observes the server lease after hostd's DispatchManager has
	// accepted the matching dispatch, so it can never create a competing
	// carrier for the same machine identity.
	_ = target
	_ = ownerSessionID
	stateRoot, err := previewRuntimeStateRoot()
	if err != nil {
		return nil, previewFailure(ErrPreviewMachineNotConfigured.Error(), ErrPreviewMachineNotConfigured, err)
	}
	store, err := runtimeIdentityStore()
	if err != nil {
		return nil, previewFailure(ErrPreviewMachineNotConfigured.Error(), ErrPreviewMachineNotConfigured, err)
	}
	registration, err := store.Registration()
	if err != nil || registration.ServerURL == "" || registration.MachineID != machineID {
		if err == nil {
			err = errors.New("registered machine does not match selected machine")
		}
		return nil, previewFailure(ErrPreviewMachineNotConfigured.Error(), ErrPreviewMachineNotConfigured, err)
	}
	auth, err := machinecontrol.NewSource(machinecontrol.Config{ControlURL: registration.ServerURL, StateRoot: stateRoot})
	if err != nil {
		return nil, previewFailure(ErrPreviewMachineNotConfigured.Error(), ErrPreviewMachineNotConfigured, err)
	}
	carrier, err := preview.NewLeaseObserverCarrier(preview.LeaseObserverCarrierConfig{MachineAuth: auth})
	if err != nil {
		return nil, previewFailure(ErrPreviewCarrierUnavailable.Error(), ErrPreviewCarrierUnavailable, err)
	}
	return carrier, nil
}

func previewRuntimeStateRoot() (string, error) {
	stateRoot := strings.TrimSpace(os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT"))
	if stateRoot == "" {
		var err error
		stateRoot, err = helperconfig.DefaultStateRoot(os.Getenv)
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(stateRoot) {
		return "", errors.New("runtime state root must be absolute")
	}
	return stateRoot, nil
}

func newProductionPreviewOwnerSessionClient() (*preview.LocalOwnerSessionClient, error) {
	stateRoot, err := previewRuntimeStateRoot()
	if err != nil {
		return nil, previewFailure(ErrPreviewOwnerSessionUnavailable.Error(), ErrPreviewOwnerSessionUnavailable, err)
	}
	data, err := readOwnerOnlyFile(filepath.Join(stateRoot, "runtime", "worker-local.json"), 4096)
	if err != nil {
		return nil, previewFailure(ErrPreviewOwnerSessionUnavailable.Error(), ErrPreviewOwnerSessionUnavailable, err)
	}
	var local struct {
		Schema        string `json:"schema"`
		ListenAddress string `json:"listen_address"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&local); err != nil {
		return nil, previewFailure(ErrPreviewOwnerSessionUnavailable.Error(), ErrPreviewOwnerSessionUnavailable, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF || local.Schema != "paperboat.worker-local/v1" {
		return nil, previewFailure(ErrPreviewOwnerSessionUnavailable.Error(), ErrPreviewOwnerSessionUnavailable, err)
	}
	host, portText, err := net.SplitHostPort(strings.TrimSpace(local.ListenAddress))
	if err != nil || (host != "127.0.0.1" && host != "::1") {
		return nil, previewFailure(ErrPreviewOwnerSessionUnavailable.Error(), ErrPreviewOwnerSessionUnavailable, err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return nil, previewFailure(ErrPreviewOwnerSessionUnavailable.Error(), ErrPreviewOwnerSessionUnavailable, err)
	}
	token, err := readOwnerOnlyFile(filepath.Join(stateRoot, "runtime", "local-control-token"), 1024)
	if err != nil {
		return nil, previewFailure(ErrPreviewOwnerSessionUnavailable.Error(), ErrPreviewOwnerSessionUnavailable, err)
	}
	if strings.TrimSpace(string(token)) == "" {
		return nil, previewFailure(ErrPreviewOwnerSessionUnavailable.Error(), ErrPreviewOwnerSessionUnavailable)
	}
	client, err := preview.NewLocalOwnerSessionClient("http://"+local.ListenAddress, strings.TrimSpace(string(token)), nil)
	if err != nil {
		return nil, previewFailure(ErrPreviewOwnerSessionUnavailable.Error(), ErrPreviewOwnerSessionUnavailable, err)
	}
	return client, nil
}
