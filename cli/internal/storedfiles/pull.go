package storedfiles

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	receiver "github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/store"
	wire "github.com/pinksaucepasta/paperboat/shared/storedfiles"
	"github.com/pinksaucepasta/paperboat/shared/teambucket"
)

type URLFetcher func(context.Context, int, int) ([]teambucket.Authority, error)

type PullConfig struct {
	RequestID, ManifestDigest, MachineID, SourceMachineID, SenderAccount string
	Files                                                                []wire.File
	Metadata                                                             wire.Metadata
	Key                                                                  []byte
	Receiver                                                             *receiver.Service
	Client                                                               *http.Client
	URLs                                                                 URLFetcher
}

// Pull resumes the request's durable batch, verifies every part before committing
// plaintext and atomically publishes the verified batch into the Inbox. A repeated
// call after publication returns without creating another Inbox file.
func Pull(ctx context.Context, c PullConfig) error {
	if ctx == nil || c.Receiver == nil || c.URLs == nil || !validContext(c.RequestID, c.ManifestDigest) || c.MachineID == "" || c.SourceMachineID == "" || c.SenderAccount == "" {
		return ErrInvalid
	}
	if ManifestDigest(c.Files) != c.ManifestDigest {
		return ErrInvalid
	}
	aead, err := newCipher(c.Key)
	if err != nil {
		return err
	}
	if err := validateParts(c.Files, c.Metadata.Parts, aead); err != nil {
		return err
	}
	transfers, err := prepareReceiver(ctx, c)
	if err != nil {
		return err
	}
	var urls []teambucket.Authority
	first := -1
	for i, part := range c.Metadata.Parts {
		transfer := &transfers[part.File]
		end := part.Offset + part.Bytes - int64(aead.NonceSize()+aead.Overhead())
		if transfer.CommittedOffset >= end && (end > 0 || transfer.State == "published") {
			continue
		}
		if transfer.State != "created" && transfer.State != "uploading" {
			return ErrInvalid
		}
		if first < 0 || i >= first+len(urls) {
			count := min(teambucket.MaxSegmentsPerBatch, len(c.Metadata.Parts)-i)
			urls, err = c.URLs(ctx, i, count)
			if err != nil {
				return fmt.Errorf("get stored file download addresses: %w", err)
			}
			if len(urls) != count {
				return ErrInvalid
			}
			first = i
		}
		plain, err := download(ctx, c.Client, urls[i-first], part, aead, c.RequestID, c.ManifestDigest)
		if err != nil {
			return err
		}
		err = appendPart(ctx, c.Receiver, transfer, part.Offset, plain)
		clear(plain)
		if err != nil {
			return err
		}
	}
	if _, err := c.Receiver.Complete(ctx, transfers[0].ID); err != nil {
		return fmt.Errorf("verify stored files: %w", err)
	}
	for _, transfer := range transfers {
		if _, err := c.Receiver.PublishedPath(ctx, transfer.ID); err != nil {
			return fmt.Errorf("deliver stored file to Inbox: %w", err)
		}
	}
	return nil
}

func validateParts(files []wire.File, parts []wire.Part, aead cipher.AEAD) error {
	if len(files) < 1 || len(files) > receiver.MaxBatchFiles || len(parts) < len(files) || len(parts) > wire.MaxParts {
		return ErrInvalid
	}
	fileIndex, offset := 0, int64(0)
	overhead := int64(aead.NonceSize() + aead.Overhead())
	for i, part := range parts {
		if part.Index != i || part.File != fileIndex || part.Offset != offset || part.Bytes < overhead || part.Bytes > teambucket.MaxObjectBytes || !validContext("part", part.SHA256) {
			return ErrInvalid
		}
		plain := part.Bytes - overhead
		if plain > files[fileIndex].Size-offset || plain == 0 && files[fileIndex].Size != 0 {
			return ErrInvalid
		}
		offset += plain
		if offset == files[fileIndex].Size {
			fileIndex++
			offset = 0
		}
		if fileIndex == len(files) && i != len(parts)-1 {
			return ErrInvalid
		}
	}
	if fileIndex != len(files) || offset != 0 {
		return ErrInvalid
	}
	return nil
}

func prepareReceiver(ctx context.Context, c PullConfig) ([]store.FileTransfer, error) {
	batch := "stored:" + c.RequestID
	transfers, err := c.Receiver.Batch(ctx, batch)
	if errors.Is(err, store.ErrNotFound) {
		files := make([]receiver.File, len(c.Files))
		for i, file := range c.Files {
			files[i] = receiver.File{Basename: file.Basename, Size: file.Size, SHA256: file.SHA256}
		}
		return c.Receiver.Create(ctx, receiver.CreateRequest{BatchID: batch, SourceMachineID: c.SourceMachineID, DestinationMachineID: c.MachineID, InitiatingUserID: c.SenderAccount, Files: files})
	}
	if err != nil {
		return nil, err
	}
	if len(transfers) != len(c.Files) {
		return nil, ErrInvalid
	}
	// The store returns a stable ID order, which need not match manifest order.
	ordered := make([]store.FileTransfer, len(transfers))
	used := make([]bool, len(transfers))
	for i, file := range c.Files {
		found := false
		for j, transfer := range transfers {
			if !used[j] && transfer.Basename == file.Basename && transfer.Size == file.Size && transfer.SHA256 == file.SHA256 && transfer.SourceMachineID == c.SourceMachineID && transfer.DestinationMachineID == c.MachineID && transfer.InitiatingUserID == c.SenderAccount {
				ordered[i] = transfer
				used[j] = true
				found = true
				break
			}
		}
		if !found {
			return nil, ErrInvalid
		}
	}
	return ordered, nil
}

func download(ctx context.Context, client *http.Client, authority teambucket.Authority, part wire.Part, aead cipher.AEAD, id, manifest string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, authority.URL, nil)
	if err != nil {
		return nil, errors.New("the stored file download address is invalid")
	}
	for name, value := range authority.Headers {
		request.Header.Set(name, value)
	}
	if client == nil {
		client = &http.Client{}
	}
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("bucket redirects are refused") }
	response, err := bounded.Do(request)
	if err != nil {
		return nil, errors.New("download stored file: the bucket did not answer; retry when it is reachable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download stored file: the bucket answered %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, part.Bytes+1))
	if err != nil {
		return nil, errors.New("download stored file: the bucket response was interrupted; retry")
	}
	if int64(len(data)) != part.Bytes || digest(data) != part.SHA256 {
		return nil, ErrInvalid
	}
	nonce := data[:aead.NonceSize()]
	plain, err := aead.Open(nil, nonce, data[aead.NonceSize():], partAD(id, manifest, part))
	if err != nil {
		return nil, ErrInvalid
	}
	return plain, nil
}

func appendPart(ctx context.Context, service *receiver.Service, transfer *store.FileTransfer, offset int64, plain []byte) error {
	if transfer.CommittedOffset < offset || transfer.CommittedOffset > offset+int64(len(plain)) {
		return ErrInvalid
	}
	remaining := plain[transfer.CommittedOffset-offset:]
	for len(remaining) > 0 {
		chunk := remaining[:min(len(remaining), protocol.FileTransferChunkBytes)]
		next, err := service.AppendVerified(ctx, transfer.ID, transfer.CommittedOffset, bytes.NewReader(chunk), sha256.Sum256(chunk))
		if err != nil {
			return fmt.Errorf("save stored file: %w", err)
		}
		*transfer = next
		remaining = remaining[len(chunk):]
	}
	return nil
}
