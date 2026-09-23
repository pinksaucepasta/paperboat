package filetransfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

// NativeClient sends application records exclusively through an authenticated
// endpoint stream. The opener owns native session admission and reconnection;
// HTTP never dials a public endpoint or falls back to a resource relay.
type NativeClient struct{ *Client }

func NewNativeClient(endpoint string, auth Auth, binding Binding, open func(context.Context) (net.Conn, error)) (*NativeClient, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") || auth.Token == "" || binding.SourceMachineID == "" || binding.DestinationMachineID == "" || binding.InitiatingUserID == "" || binding.SourceMachineID == binding.DestinationMachineID {
		return nil, errors.New("invalid native file transfer configuration")
	}
	transport, err := NewNativeRoundTripper(open)
	if err != nil {
		return nil, err
	}
	client := NewClient(endpoint, auth, binding, &http.Client{Transport: transport, Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})
	client.native = true
	return &NativeClient{Client: client}, nil
}

func (c *Client) uploadNative(ctx context.Context, manifest Manifest, source Source) error {
	var last error
	previous := int64(-1)
	for failures := 0; failures < 4; {
		if err := ctx.Err(); err != nil {
			return err
		}
		offset, err := c.Offset(ctx, manifest.TransferID)
		if err == nil {
			if offset == source.Size {
				return nil
			}
			if offset < 0 || offset > source.Size {
				return errors.New("invalid committed transfer offset")
			}
			if offset <= previous {
				return errors.New("committed transfer offset did not advance")
			}
			err = c.patchNative(ctx, manifest.TransferID, offset, source)
			if err == nil {
				previous = offset
				failures = 0
				continue
			}
		}
		var remote *Error
		if errors.As(err, &remote) && !transientHTTPStatus(remote.StatusCode) {
			return err
		}
		last = err
		if err := waitOperationRetry(ctx, failures); err != nil {
			return err
		}
		failures++
	}
	return last
}

func (c *Client) patchNative(ctx context.Context, id string, offset int64, source Source) error {
	if _, err := source.Reader.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	chunk := make([]byte, min(source.Size-offset, int64(protocol.FileTransferChunkBytes)))
	if _, err := io.ReadFull(source.Reader, chunk); err != nil {
		return err
	}
	digest := sha256.Sum256(chunk)
	headers := http.Header{"Upload-Digest": {"sha256=" + hex.EncodeToString(digest[:])}}
	response, err := c.requestWithBody(ctx, http.MethodPatch, c.Endpoint+"/"+id+"/content", operationID("patch", id), contentType, offset, func() (io.Reader, error) { return bytes.NewReader(chunk), nil }, headers)
	if response != nil {
		_ = response.Body.Close()
	}
	return err
}

// SendBatch also resumes an existing batch. Its ID and source manifests must be
// unchanged after a process restart; the receiver's durable offset is authoritative.
func (c *NativeClient) SendBatch(ctx context.Context, batchID, sessionID string, sources []Source) (Batch, error) {
	if c == nil || c.Client == nil || !c.native || batchID == "" {
		return Batch{}, errors.New("invalid native file transfer")
	}
	return c.sendBatchPlaintext(ctx, batchID, sessionID, sources)
}

// ResumeRequiredError distinguishes an interrupted live transfer from explicit
// cancellation. Only the endpoints retain the bounded partial data.
type ResumeRequiredError struct {
	BatchID string
	Err     error
}

func (e *ResumeRequiredError) Error() string {
	return "file transfer interrupted; resume the same batch with the original files"
}
func (e *ResumeRequiredError) Unwrap() error { return e.Err }

func (c *Client) failedBatch(ctx context.Context, batch Batch, err error) (Batch, error) {
	var remote *Error
	if c.native && !errors.Is(ctx.Err(), context.Canceled) && (!errors.As(err, &remote) || transientHTTPStatus(remote.StatusCode)) {
		return batch, &ResumeRequiredError{BatchID: batch.BatchID, Err: err}
	}
	c.cancelBatch(batch.Transfers)
	return batch, err
}
