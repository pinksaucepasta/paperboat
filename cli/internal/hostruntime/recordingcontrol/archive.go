package recordingcontrol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/recordings"
	"github.com/pinksaucepasta/paperboat/shared/backoff"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"github.com/pinksaucepasta/paperboat/shared/teambucket"
)

const (
	archiveInterval = 30 * time.Second
	// noBucketWait is how long a team without a recordings bucket is left alone before asking again.
	noBucketWait   = 10 * time.Minute
	retryBase      = 5 * time.Second
	retryMax       = 15 * time.Minute
	batchesPerPass = 4
	putLimit       = 90 * time.Second
)

// ErrNoBucket: the team has no bucket chosen for recordings.
var ErrNoBucket = errors.New("the team has no bucket for recordings")

// Archive is what the machine asks Paperboat for to copy closed segments to the team bucket. Segment files are
// already ciphertext; Paperboat only signs where they may go.
type Archive interface {
	UploadURLs(ctx context.Context, recordingID string, segments []teambucket.Segment) ([]teambucket.Authority, error)
	Archived(ctx context.Context, recordingID string, segments int) error
}

// Put sends one segment file to its presigned URL.
type Put func(ctx context.Context, upload teambucket.Authority, data []byte) error

func (c *Controller) archiveLoop(ctx context.Context) {
	defer close(c.archiveDone)
	ticker := time.NewTicker(archiveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.archive(ctx)
		}
	}
}

// archive copies segments that closed since the last pass to the team bucket, oldest recording first, a bounded
// number of batches per pass. A failure keeps the position: the next pass resumes there, later with growing waits.
func (c *Controller) archive(ctx context.Context) {
	if c.config.Archive == nil || c.config.Put == nil || c.config.Now().Before(c.archiveNext) {
		return
	}
	metas, err := c.config.Sessions.Recordings()
	if err != nil {
		return
	}
	for left, i := batchesPerPass, len(metas)-1; i >= 0 && left > 0; i-- { // List is newest first
		meta := metas[i]
		for left > 0 && c.archivedCount(meta.ID) < int(meta.Segments) {
			left--
			if err := c.archiveBatch(ctx, meta); err != nil {
				c.archiveFailed(err)
				return
			}
		}
	}
	c.archiveFailures, c.archiveNext = 0, time.Time{}
}

func (c *Controller) archiveFailed(err error) {
	if errors.Is(err, ErrNoBucket) {
		c.archiveNext = c.config.Now().Add(noBucketWait)
		return
	}
	c.archiveFailures++
	c.archiveNext = c.config.Now().Add(backoff.Retry(c.archiveFailures, retryBase, retryMax))
	diagnosticlog.TryInfo("recording segments could not be copied to the team bucket; retrying", "failures", fmt.Sprint(c.archiveFailures))
}

func (c *Controller) archivedCount(id string) int {
	c.ledger.mu.Lock()
	defer c.ledger.mu.Unlock()
	return c.ledger.Archived[id]
}

func (c *Controller) archiveBatch(ctx context.Context, meta recordings.Meta) error {
	first := c.archivedCount(meta.ID)
	last := min(int(meta.Segments), first+teambucket.MaxSegmentsPerBatch)
	files := make([][]byte, 0, last-first)
	specs := make([]teambucket.Segment, 0, last-first)
	for index := first; index < last; index++ {
		data, err := c.config.Sessions.RecordingSegment(meta.ID, uint32(index))
		if err != nil {
			return fmt.Errorf("read segment %d: %w", index, err)
		}
		sum := sha256.Sum256(data)
		files, specs = append(files, data), append(specs, teambucket.Segment{Index: index, Bytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
	}
	uploads, err := c.config.Archive.UploadURLs(ctx, meta.ID, specs)
	if err != nil {
		return err
	}
	if len(uploads) != len(files) {
		return errors.New("Paperboat answered with the wrong number of upload addresses")
	}
	for i, upload := range uploads {
		putCtx, cancel := context.WithTimeout(ctx, putLimit)
		err := c.config.Put(putCtx, upload, files[i])
		cancel()
		if err != nil {
			return fmt.Errorf("upload segment %d: %w", first+i, err)
		}
	}
	if err := c.config.Archive.Archived(ctx, meta.ID, last); err != nil {
		return err
	}
	c.ledger.mu.Lock()
	defer c.ledger.mu.Unlock()
	c.ledger.Archived[meta.ID] = last
	if err := c.ledger.save(); err != nil {
		diagnosticlog.TryWarn("the recording ledger could not be saved; segments may be uploaded again")
	}
	return nil
}

// UploadURLs, Archived: the Archive endpoints, called with the machine's own credential and proof.
func (f *Fetcher) UploadURLs(ctx context.Context, recordingID string, segments []teambucket.Segment) ([]teambucket.Authority, error) {
	var out teambucket.UploadURLs
	status, err := f.call(ctx, http.MethodPost, teambucket.UploadURLsPath(recordingID), teambucket.UploadRequest{Segments: segments}, &out)
	if status == http.StatusConflict {
		return nil, ErrNoBucket
	}
	return out.Uploads, err
}

func (f *Fetcher) Archived(ctx context.Context, recordingID string, segments int) error {
	_, err := f.call(ctx, http.MethodPost, teambucket.ArchivedPath(recordingID), teambucket.ArchivedRequest{Segments: segments}, &struct{}{})
	return err
}

// PutObject is the production Put: one PUT with exactly the headers the server signed. The bucket's answer is
// the team's own business, so only its status is reported.
func PutObject(client *http.Client) Put {
	return func(ctx context.Context, upload teambucket.Authority, data []byte) error {
		request, err := http.NewRequestWithContext(ctx, http.MethodPut, upload.URL, bytes.NewReader(data))
		if err != nil {
			return err
		}
		for name, value := range upload.Headers {
			request.Header.Set(name, value)
		}
		request.ContentLength = int64(len(data))
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("the bucket did not answer: %w", err)
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		if response.StatusCode < 200 || response.StatusCode > 299 {
			return fmt.Errorf("the bucket answered %d", response.StatusCode)
		}
		return nil
	}
}
