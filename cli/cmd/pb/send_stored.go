package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/cli/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/recordingcontrol"
	"github.com/pinksaucepasta/paperboat/cli/internal/storedfiles"
	wire "github.com/pinksaucepasta/paperboat/shared/storedfiles"
	"github.com/pinksaucepasta/paperboat/shared/teambucket"
	"github.com/spf13/cobra"
)

// sendStoredFiles uploads client-side ciphertext before returning. The recipient
// can accept and receive it later without retaining a connection to this sender.
func sendStoredFiles(ctx context.Context, command *cobra.Command, client *api.Client, request api.TeamInboxRequest, sources []filetransfer.Source, destination string, jsonOutput bool) (resultErr error) {
	identityStore, err := runtimeIdentityStore()
	if err != nil {
		return fmt.Errorf("open source machine identity: %w", err)
	}
	signer := identityStore.Current()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	defer clear(key)
	recipients, err := client.StoredFileRecipients(ctx, request.RequestID)
	if err != nil {
		return friendlyCommandError(err)
	}
	if len(recipients) < 1 || len(recipients) > wire.MaxRecipients {
		return errors.New("the recipient has no machine keys for stored files; ask them to pair a machine, then retry")
	}
	metadata := wire.Metadata{WriterPublic: signer.Public(), SealedKeys: make(map[string][]byte, len(recipients))}
	for _, recipient := range recipients {
		if _, exists := metadata.SealedKeys[recipient.MachineID]; exists {
			return errors.New("the recipient's machine keys changed; retry the transfer")
		}
		claims := environmente2ee.StoredFileClaims{RequestID: request.RequestID, ManifestDigest: request.ManifestDigest, MachineID: recipient.MachineID, HostPublic: recipient.HostPublic, WriterPublic: metadata.WriterPublic}
		sealed, err := environmente2ee.SealStoredFileKey(ctx, claims, key, signer.Sign)
		if err != nil {
			return fmt.Errorf("seal file key for recipient machine: %w", err)
		}
		metadata.SealedKeys[recipient.MachineID] = sealed
	}
	prepared, err := storedfiles.Prepare(ctx, request.RequestID, request.ManifestDigest, sources, key, os.TempDir())
	if err != nil {
		return fmt.Errorf("prepare encrypted stored files: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, prepared.Close()) }()
	metadata.Parts = prepared.Parts
	if err := client.PutStoredFile(ctx, request.RequestID, metadata); err != nil {
		return friendlyCommandError(err)
	}
	//paperboat:allow-source-policy default-http owner=send-stored reason=per-part-context-timeout-no-redirect
	bucket := &http.Client{Transport: http.DefaultTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("bucket redirects are refused") }}
	put := recordingcontrol.PutObject(bucket)
	for first := 0; first < len(prepared.Parts); first += teambucket.MaxSegmentsPerBatch {
		count := min(teambucket.MaxSegmentsPerBatch, len(prepared.Parts)-first)
		uploads, err := client.StoredFileUploads(ctx, request.RequestID, first, count)
		if err != nil {
			return friendlyCommandError(err)
		}
		if len(uploads.URLs) != count {
			return errors.New("the server returned an incomplete set of stored file upload addresses; retry the transfer")
		}
		for i, authority := range uploads.URLs {
			data, err := prepared.ReadPart(first + i)
			if err != nil {
				return fmt.Errorf("read encrypted stored file: %w", err)
			}
			uploadContext, cancel := context.WithTimeout(ctx, 90*time.Second)
			err = put(uploadContext, authority, data)
			cancel()
			if err != nil {
				// http.Client wraps network and redirect errors with the full presigned URL.
				var addressError *url.Error
				if errors.As(err, &addressError) {
					err = errors.New("the bucket did not answer; check the connection and retry")
				}
				return fmt.Errorf("upload stored file part %d: %w", first+i+1, err)
			}
		}
	}
	if err := client.StoredFileUploaded(ctx, request.RequestID); err != nil {
		return friendlyCommandError(err)
	}
	if jsonOutput {
		return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": map[string]any{"request_id": request.RequestID, "team_id": request.TeamID, "destination_machine_id": request.DestinationMachineID, "status": "pending", "stored": true, "uploaded": true, "expires_at": request.ExpiresAt}})
	}
	_, err = fmt.Fprintf(command.OutOrStdout(), "Files are waiting for %s to accept (%s). You can disconnect now.\n", destination, request.RequestID)
	return err
}
