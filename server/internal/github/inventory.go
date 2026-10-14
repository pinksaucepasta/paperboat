package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"

	gh "github.com/google/go-github/v88/github"
	"github.com/pinksaucepasta/paperboat-server/internal/secrets"
)

type repositoryCursor struct {
	Account     string `json:"account"`
	Page, Limit int
	Private     bool
}
type repositoryPager interface {
	ListReposPage(context.Context, string, int, int, bool) ([]Repo, int, error)
}
type repositoryByID interface {
	GetRepoByID(context.Context, string, string) (Repo, error)
}

func (s *Service) RepositoryInventory(ctx context.Context, account, cursor string, limit int, private bool) ([]Repo, string, error) {
	if account == "" || limit < 1 || limit > 100 {
		return nil, "", ErrRepoNotFound
	}
	page := 1
	if cursor != "" {
		encrypted, decodeErr := base64.RawURLEncoding.DecodeString(cursor)
		if decodeErr != nil || len(cursor) > 4096 {
			return nil, "", ErrRepoNotFound
		}
		raw, err := secrets.Decrypt(s.cfg.Secrets.EncryptionKey, encrypted)
		if err != nil {
			return nil, "", ErrRepoNotFound
		}
		var value repositoryCursor
		if json.Unmarshal([]byte(raw), &value) != nil || value.Account != account || value.Limit != limit || value.Private != private || value.Page < 1 {
			return nil, "", ErrRepoNotFound
		}
		page = value.Page
	}
	token, _, err := s.githubToken(ctx, account)
	if err != nil {
		return nil, "", err
	}
	pager, ok := s.client.(repositoryPager)
	if !ok {
		return nil, "", ErrRepositoryAccessUnavailable
	}
	items, next, err := pager.ListReposPage(ctx, token, page, limit, private)
	if err != nil {
		return nil, "", err
	}
	nextCursor := ""
	if next > 0 {
		raw, _ := json.Marshal(repositoryCursor{Account: account, Page: next, Limit: limit, Private: private})
		encrypted, encryptErr := secrets.Encrypt(s.cfg.Secrets.EncryptionKey, string(raw))
		err = encryptErr
		nextCursor = base64.RawURLEncoding.EncodeToString(encrypted)
	}
	return items, nextCursor, err
}
func (c HTTPClient) ListReposPage(ctx context.Context, token string, page, limit int, private bool) ([]Repo, int, error) {
	client, err := c.sdkClient(token)
	if err != nil {
		return nil, 0, err
	}
	visibility := "all"
	if private {
		visibility = "private"
	}
	items, response, err := client.Repositories.ListByAuthenticatedUser(ctx, &gh.RepositoryListByAuthenticatedUserOptions{Affiliation: "owner,collaborator,organization_member", Visibility: visibility, Sort: "full_name", Direction: "asc", ListOptions: gh.ListOptions{Page: page, PerPage: limit}})
	if err != nil {
		return nil, 0, normalizeGitHubError(err)
	}
	out := make([]Repo, 0, len(items))
	for _, item := range items {
		out = append(out, repoFromSDK(item))
	}
	next := 0
	if response != nil {
		next = response.NextPage
	}
	return out, next, nil
}
func (c HTTPClient) GetRepoByID(ctx context.Context, token, id string) (Repo, error) {
	number, err := strconv.ParseInt(id, 10, 64)
	if err != nil || number < 1 {
		return Repo{}, ErrRepoNotFound
	}
	client, err := c.sdkClient(token)
	if err != nil {
		return Repo{}, err
	}
	item, _, err := client.Repositories.GetByID(ctx, number)
	if err != nil {
		return Repo{}, normalizeGitHubError(err)
	}
	return repoFromSDK(item), nil
}
func (s *Service) repositoryByID(ctx context.Context, token, id string) (Repo, error) {
	if client, ok := s.client.(repositoryByID); ok {
		return client.GetRepoByID(ctx, token, id)
	}
	// Test/provider adapters without immutable-ID lookup still resolve against current access.
	items, err := s.client.ListRepos(ctx, token)
	if err != nil {
		return Repo{}, err
	}
	for _, item := range items {
		if item.ID == id {
			return item, nil
		}
	}
	return Repo{}, ErrRepoNotFound
}
func (f *FakeClient) ListReposPage(ctx context.Context, token string, page, limit int, private bool) ([]Repo, int, error) {
	items, err := f.ListRepos(ctx, token)
	if err != nil {
		return nil, 0, err
	}
	filtered := []Repo{}
	for _, item := range items {
		if !private || item.Private {
			filtered = append(filtered, item)
		}
	}
	start := (page - 1) * limit
	if start >= len(filtered) {
		return []Repo{}, 0, nil
	}
	end := start + limit
	next := 0
	if end < len(filtered) {
		next = page + 1
	} else {
		end = len(filtered)
	}
	return filtered[start:end], next, nil
}
