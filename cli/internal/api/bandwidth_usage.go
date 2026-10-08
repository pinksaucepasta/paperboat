package api

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// NativeUsageReport is an absolute application-byte counter for one authorized
// stream, UTC minute and observed path. The server owns resource attribution.
type NativeUsageReport struct {
	AccessSessionID string    `json:"access_session_id"`
	StreamID        string    `json:"stream_id"`
	Consumer        string    `json:"consumer"`
	Mode            string    `json:"mode"`
	NodeID          string    `json:"node_id"`
	IntervalStart   time.Time `json:"interval_start"`
	IntervalEnd     time.Time `json:"interval_end"`
	UploadBytes     int64     `json:"upload_bytes"`
	DownloadBytes   int64     `json:"download_bytes"`
}

func (c *Client) ReportNativeUsage(ctx context.Context, reports []NativeUsageReport, unrecordedBytes, expiredBytes int64, incomplete bool) error {
	return c.doWithHeaders(ctx, http.MethodPost, "/v1/usage/native", struct {
		Reports             []NativeUsageReport `json:"reports"`
		UnrecordedBytes     int64               `json:"unrecorded_bytes"`
		ExpiredBytes        int64               `json:"expired_bytes"`
		RecordingIncomplete bool                `json:"recording_incomplete"`
	}{reports, unrecordedBytes, expiredBytes, incomplete}, nil, http.Header{"Idempotency-Key": []string{"operation_" + uuid.NewString()}})
}
