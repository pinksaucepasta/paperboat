package api

import (
	"context"
	wire "github.com/pinksaucepasta/paperboat/shared/storedfiles"
	"net/http"
	"net/url"
)

func storedRequestPath(id string) string {
	return "/v1/team-inbox/requests/" + url.PathEscape(id) + "/stored/"
}
func (c *Client) StoredFileRecipients(ctx context.Context, id string) ([]wire.Recipient, error) {
	var out struct {
		Recipients []wire.Recipient `json:"recipients"`
	}
	err := c.do(ctx, http.MethodGet, storedRequestPath(id)+"recipients", nil, &out)
	return out.Recipients, err
}
func (c *Client) PutStoredFile(ctx context.Context, id string, m wire.Metadata) error {
	return c.do(ctx, http.MethodPost, storedRequestPath(id)+"metadata", m, &struct {
		Stored bool `json:"stored"`
	}{})
}
func (c *Client) StoredFileUploads(ctx context.Context, id string, first, count int) (wire.URLs, error) {
	var out wire.URLs
	err := c.do(ctx, http.MethodPost, storedRequestPath(id)+"upload-urls", wire.Range{First: first, Count: count}, &out)
	return out, err
}
func (c *Client) StoredFileUploaded(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, storedRequestPath(id)+"uploaded", struct{}{}, &struct {
		Uploaded bool `json:"uploaded"`
	}{})
}

func (c *Client) MachineStoredFiles(ctx context.Context, operation string) (wire.Page, error) {
	var out wire.Page
	err := c.doWithHeaders(ctx, http.MethodGet, wire.MachinePath, nil, &out, http.Header{"Idempotency-Key": {operation}})
	return out, err
}
func (c *Client) MachineStoredDownloads(ctx context.Context, operation, id string, first, count int) (wire.URLs, error) {
	var out wire.URLs
	err := c.doWithHeaders(ctx, http.MethodPost, wire.MachinePath+"/"+url.PathEscape(id)+"/download-urls", wire.Range{First: first, Count: count}, &out, http.Header{"Idempotency-Key": {operation}})
	return out, err
}
func (c *Client) MachineStoredReceived(ctx context.Context, operation, id, digest string) error {
	return c.doWithHeaders(ctx, http.MethodPost, wire.MachinePath+"/"+url.PathEscape(id)+"/received", map[string]string{"manifest_digest": digest}, &struct {
		Received bool `json:"received"`
	}{}, http.Header{"Idempotency-Key": {operation}})
}
