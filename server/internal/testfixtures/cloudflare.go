package testfixtures

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Cloudflare writes the test records with the server's own DNS token (resolved per request, never stored or logged).
type Cloudflare struct {
	BaseURL string // default https://api.cloudflare.com/client/v4
	Token   func(context.Context) (string, error)
	HTTP    *http.Client
}

type cloudflareAnswer struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code int `json:"code"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

type cloudflareRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Comment string `json:"comment"`
}

func (c *Cloudflare) call(ctx context.Context, method, path string, body any) (cloudflareAnswer, int, error) {
	var answer cloudflareAnswer
	token, err := c.Token(ctx)
	if err != nil || token == "" {
		return answer, 0, errors.New("the DNS provider token is unavailable")
	}
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://api.cloudflare.com/client/v4"
	}
	request, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return answer, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(request)
	if err != nil {
		return answer, 0, fmt.Errorf("the DNS provider is unreachable: %w", errors.Unwrap(err))
	}
	defer response.Body.Close()
	_ = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&answer)
	return answer, response.StatusCode, nil
}

func (c *Cloudflare) Create(ctx context.Context, zoneID string, r Record) (string, error) {
	answer, status, err := c.call(ctx, http.MethodPost, "/zones/"+zoneID+"/dns_records", map[string]any{"type": r.Type, "name": r.Name, "content": r.Content, "ttl": 60, "proxied": false, "comment": r.Comment})
	if err != nil {
		return "", err
	}
	var created cloudflareRecord
	if !answer.Success || json.Unmarshal(answer.Result, &created) != nil || created.ID == "" {
		return "", fmt.Errorf("the DNS provider refused the record (HTTP %d)", status)
	}
	return created.ID, nil
}

func (c *Cloudflare) Get(ctx context.Context, zoneID, id string) (Record, bool, error) {
	answer, status, err := c.call(ctx, http.MethodGet, "/zones/"+zoneID+"/dns_records/"+id, nil)
	if err != nil {
		return Record{}, false, err
	}
	if status == http.StatusNotFound {
		return Record{}, false, nil
	}
	var found cloudflareRecord
	if !answer.Success || json.Unmarshal(answer.Result, &found) != nil {
		return Record{}, false, fmt.Errorf("the DNS provider could not read the record (HTTP %d)", status)
	}
	return Record(found), true, nil
}

func (c *Cloudflare) Delete(ctx context.Context, zoneID, id string) error {
	answer, status, err := c.call(ctx, http.MethodDelete, "/zones/"+zoneID+"/dns_records/"+id, nil)
	if err != nil {
		return err
	}
	if !answer.Success && status != http.StatusNotFound {
		return fmt.Errorf("the DNS provider could not delete the record (HTTP %d)", status)
	}
	return nil
}
