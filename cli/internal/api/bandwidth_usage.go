package api

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/shared/nativeusage"
)

// NativeUsageReport is an absolute application-byte counter for one authorized stream, UTC minute and observed
// path. The server owns resource attribution.
type NativeUsageReport = nativeusage.Report

func (c *Client) ReportNativeUsage(ctx context.Context, reports []NativeUsageReport, unrecordedBytes, expiredBytes int64, incomplete bool) error {
	return c.doWithHeaders(ctx, http.MethodPost, "/v1/usage/native", nativeusage.Batch{Reports: reports, UnrecordedBytes: unrecordedBytes, ExpiredBytes: expiredBytes, RecordingIncomplete: incomplete}, nil, http.Header{"Idempotency-Key": []string{"operation_" + uuid.NewString()}})
}
