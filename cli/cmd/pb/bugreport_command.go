package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/api"
	bugreportpkg "github.com/pinksaucepasta/paperboat/internal/bugreport"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"github.com/spf13/cobra"
)

func bugreportCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "bugreport",
		Short: "Create a redacted Paperboat diagnostic bundle",
		Args:  commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			jsonOutput, _ := command.Flags().GetBool("json")
			record, _ := command.Flags().GetBool("record")
			upload, _ := command.Flags().GetBool("upload")
			localClient, _, err := localDaemonSnapshot(command, installLocalDaemonService)
			if err != nil {
				return fmt.Errorf("start local Paperboat daemon: %w", err)
			}
			var server bugreportpkg.Server
			if upload {
				ctx := actionContext(command, nil)
				d, depErr := buildDeps(ctx)
				if depErr != nil {
					return depErr
				}
				server, err = newRefreshingBugreportServer(d.cfg.ServerURL, d.auth)
				if err != nil {
					return err
				}
			}
			promptOutput := command.ErrOrStderr()
			beforeUpload := func(result bugreportpkg.Result) error {
				_, writeErr := fmt.Fprintf(command.ErrOrStderr(), "Uploading redacted categories: %s (%d bytes)\n", strings.Join(result.Categories, ", "), result.Bytes)
				return writeErr
			}
			if jsonOutput {
				promptOutput = io.Discard
				beforeUpload = func(bugreportpkg.Result) error { return nil }
			}
			result, runErr := bugreportpkg.Run(command.Context(), bugreportpkg.Options{
				Record: record, Upload: upload, Input: command.InOrStdin(), Prompt: promptOutput,
				Local: localClient, Server: server,
				BeforeUpload: beforeUpload,
			})
			if jsonOutput {
				if runErr != nil {
					result.Error = bugreportJSONFailure(runErr, result)
					result.Error.SupportReference = supportref.FromContext(command.Context())
					var remote *api.APIError
					if errors.As(runErr, &remote) && supportref.Valid(remote.SupportReference) {
						result.Error.SupportReference = remote.SupportReference
					}
				}
				encoder := json.NewEncoder(command.OutOrStdout())
				encoder.SetEscapeHTML(false)
				if encodeErr := encoder.Encode(result); encodeErr != nil {
					return encodeErr
				}
				if runErr != nil {
					return presentedCommandFailure{cause: runErr}
				}
				return nil
			}
			if runErr != nil {
				if result.BundleCreated {
					return fmt.Errorf("%w; local bundle remains at %s", runErr, result.BundlePath)
				}
				return runErr
			}
			writeBugreportResult(command.OutOrStdout(), result)
			return nil
		},
	}
	command.Flags().Bool("record", false, "record reproduction start and end markers")
	command.Flags().Bool("upload", false, "upload the exact redacted bundle")
	command.Flags().Bool("json", false, "print JSON")
	return command
}

type refreshingBugreportServer struct {
	serverURL string
	auth      config.AuthSource
	client    *api.Client
}

func newRefreshingBugreportServer(serverURL string, auth config.AuthSource) (*refreshingBugreportServer, error) {
	credential, err := auth.Credential()
	if err != nil {
		return nil, err
	}
	return &refreshingBugreportServer{serverURL: serverURL, auth: auth, client: api.New(serverURL, credential, nil)}, nil
}

func (s *refreshingBugreportServer) refresh() (bool, error) {
	refresher, ok := s.auth.(interface {
		Refresh() (config.Credential, error)
	})
	if !ok {
		return false, nil
	}
	credential, err := refresher.Refresh()
	if err != nil {
		return false, err
	}
	s.client = api.New(s.serverURL, credential, nil)
	return true, nil
}

func (s *refreshingBugreportServer) CreateDiagnosticUploadIntent(ctx context.Context, key string, request api.DiagnosticUploadIntentRequest) (api.DiagnosticUploadIntent, error) {
	result, err := s.client.CreateDiagnosticUploadIntent(ctx, key, request)
	if errors.Is(err, api.ErrUnauthenticated) {
		refreshed, refreshErr := s.refresh()
		if refreshErr != nil {
			return result, refreshErr
		}
		if refreshed {
			return s.client.CreateDiagnosticUploadIntent(ctx, key, request)
		}
	}
	return result, err
}

func (s *refreshingBugreportServer) UploadDiagnosticBundle(ctx context.Context, intent api.DiagnosticUploadIntent, content io.Reader, bytes int64) error {
	return s.client.UploadDiagnosticBundle(ctx, intent, content, bytes)
}

func (s *refreshingBugreportServer) CompleteDiagnosticUploadIntent(ctx context.Context, intentID string) (api.DiagnosticUploadIntent, error) {
	result, err := s.client.CompleteDiagnosticUploadIntent(ctx, intentID)
	if errors.Is(err, api.ErrUnauthenticated) {
		refreshed, refreshErr := s.refresh()
		if refreshErr != nil {
			return result, refreshErr
		}
		if refreshed {
			return s.client.CompleteDiagnosticUploadIntent(ctx, intentID)
		}
	}
	return result, err
}

func writeBugreportResult(output io.Writer, result bugreportpkg.Result) {
	fmt.Fprintf(output, "Bundle: %s\n", result.BundlePath)
	fmt.Fprintf(output, "Categories: %s\n", strings.Join(result.Categories, ", "))
	fmt.Fprintf(output, "Size: %d bytes\n", result.Bytes)
	if result.Uploaded {
		fmt.Fprintf(output, "Server correlation: %s\n", result.ServerCorrelationID)
	}
}

func bugreportJSONFailure(err error, result bugreportpkg.Result) *bugreportpkg.Failure {
	stage := "bugreport"
	var stageErr *bugreportpkg.StageError
	if errors.As(err, &stageErr) {
		stage = stageErr.Stage
	}
	code, message := "bugreport_failed", "The diagnostic bundle could not be created."
	if result.Uploaded {
		code, message = "bugreport_cleanup_failed", "The redacted bundle was uploaded; closing the local bundle failed. The local copy was preserved."
	} else if result.BundleCreated {
		code, message = "bugreport_upload_failed", "The operation did not finish; the local redacted bundle was preserved."
	}
	return &bugreportpkg.Failure{Code: code, Stage: stage, Message: message}
}
