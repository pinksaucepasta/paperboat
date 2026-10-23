package teamidentity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	serverdiagnostics "github.com/pinksaucepasta/paperboat-server/internal/diagnostics"
)

// WorkOS is the part of the WorkOS API that team SSO and directory sync use (PLAN F19-02..06):
// organizations, Admin Portal links, the Events API, and SSO connections and directories.
// WorkOS owns IdP configuration; Paperboat reads state and never handles SAML or SCIM itself.
type WorkOS interface {
	CreateOrganization(ctx context.Context, name, externalID, key string) (string, error)
	DeleteOrganization(ctx context.Context, id string) error
	PortalLink(ctx context.Context, organization, intent, returnURL string) (string, error)
	Events(ctx context.Context, after string, limit int) ([]Event, string, error)
	Organization(ctx context.Context, id string) (OrganizationState, error)
}

// Event is one WorkOS event; only what routing needs is kept.
type Event struct {
	ID             string
	Type           string
	OrganizationID string
	DirectoryID    string
}

// OrganizationState is everything Paperboat mirrors about one team's organization.
type OrganizationState struct {
	Domains     []Domain
	Connections []Connection
	Directory   *Directory
	Users       []DirectoryUser
	Groups      []DirectoryGroup
}

type Domain struct {
	Domain string `json:"domain"`
	State  string `json:"state"`
}

type Connection struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	State string `json:"state"`
}

type Directory struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	State string `json:"state"`
}

type DirectoryUser struct {
	ID     string
	Email  string
	Active bool
	Groups []string
}

type DirectoryGroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// EventTypes are the WorkOS events that change a team's SSO or directory state.
var EventTypes = []string{
	"connection.activated", "connection.deactivated", "connection.deleted",
	"organization.updated", "organization_domain.verified", "organization_domain.verification_failed",
	"dsync.activated", "dsync.deleted",
	"dsync.user.created", "dsync.user.updated", "dsync.user.deleted",
	"dsync.group.created", "dsync.group.updated", "dsync.group.deleted",
	"dsync.group.user_added", "dsync.group.user_removed",
}

// MaxDirectoryUsers bounds one organization reconcile.
const MaxDirectoryUsers = 10000

type HTTPWorkOS struct {
	BaseURL, APIKey string
	Client          *http.Client
}

// ErrWorkOS carries only the endpoint and status; provider bodies are never echoed.
type ErrWorkOS struct {
	Step   string
	Status int
}

func (e ErrWorkOS) Error() string { return fmt.Sprintf("workos %s failed (HTTP %d)", e.Step, e.Status) }

const maxResponse = 4 << 20

func (c HTTPWorkOS) do(ctx context.Context, step, method, path, key string, body, out any) error {
	if strings.TrimSpace(c.BaseURL) == "" || strings.TrimSpace(c.APIKey) == "" {
		return ErrNotConfigured
	}
	client := c.Client
	if client == nil {
		client = serverdiagnostics.DefaultProviderClient("workos")
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("workos %s: %w", step, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil {
		return fmt.Errorf("workos %s: %w", step, err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return ErrWorkOS{Step: step, Status: res.StatusCode}
	}
	if len(raw) > maxResponse {
		return ErrWorkOS{Step: step + " (response too large)", Status: res.StatusCode}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return ErrWorkOS{Step: step + " (invalid response)", Status: res.StatusCode}
	}
	return nil
}

func (c HTTPWorkOS) CreateOrganization(ctx context.Context, name, externalID, key string) (string, error) {
	var out struct{ ID string }
	if err := c.do(ctx, "create organization", http.MethodPost, "/organizations", key, map[string]any{"name": name, "external_id": externalID}, &out); err != nil {
		return "", err
	}
	if !strings.HasPrefix(out.ID, "org_") {
		return "", ErrWorkOS{Step: "create organization (invalid id)", Status: 200}
	}
	return out.ID, nil
}

func (c HTTPWorkOS) DeleteOrganization(ctx context.Context, id string) error {
	err := c.do(ctx, "delete organization", http.MethodDelete, "/organizations/"+url.PathEscape(id), "", nil, nil)
	var e ErrWorkOS
	if errors.As(err, &e) && e.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// PortalLink returns a short-lived Admin Portal link for one setup intent.
func (c HTTPWorkOS) PortalLink(ctx context.Context, organization, intent, returnURL string) (string, error) {
	var out struct{ Link string }
	body := map[string]any{"organization": organization, "intent": intent, "return_url": returnURL}
	if err := c.do(ctx, "generate portal link", http.MethodPost, "/portal/generate_link", "", body, &out); err != nil {
		return "", err
	}
	if u, err := url.Parse(out.Link); err != nil || u.Scheme != "https" || u.Host == "" {
		return "", ErrWorkOS{Step: "generate portal link (invalid link)", Status: 200}
	}
	return out.Link, nil
}

// Events reads events after the cursor, oldest first, and returns the next cursor.
func (c HTTPWorkOS) Events(ctx context.Context, after string, limit int) ([]Event, string, error) {
	q := url.Values{"limit": {fmt.Sprint(limit)}}
	for _, t := range EventTypes {
		q.Add("events[]", t)
	}
	if after != "" {
		q.Set("after", after)
	}
	var out struct {
		Data []struct {
			ID    string          `json:"id"`
			Event string          `json:"event"`
			Data  json.RawMessage `json:"data"`
		} `json:"data"`
		ListMetadata struct {
			After string `json:"after"`
		} `json:"list_metadata"`
	}
	if err := c.do(ctx, "list events", http.MethodGet, "/events?"+q.Encode(), "", nil, &out); err != nil {
		return nil, "", err
	}
	events := make([]Event, 0, len(out.Data))
	next := after
	for _, e := range out.Data {
		var data struct {
			OrganizationID string `json:"organization_id"`
			DirectoryID    string `json:"directory_id"`
			ID             string `json:"id"`
			User           struct {
				OrganizationID string `json:"organization_id"`
				DirectoryID    string `json:"directory_id"`
			} `json:"user"`
		}
		_ = json.Unmarshal(e.Data, &data)
		ev := Event{ID: e.ID, Type: e.Event, OrganizationID: data.OrganizationID, DirectoryID: data.DirectoryID}
		if ev.OrganizationID == "" {
			ev.OrganizationID = data.User.OrganizationID
		}
		if ev.DirectoryID == "" {
			ev.DirectoryID = data.User.DirectoryID
		}
		if strings.HasPrefix(e.Event, "organization.") && ev.OrganizationID == "" {
			ev.OrganizationID = data.ID
		}
		if strings.HasPrefix(e.Event, "dsync.") && !strings.HasPrefix(e.Event, "dsync.user") && !strings.HasPrefix(e.Event, "dsync.group") && ev.DirectoryID == "" {
			ev.DirectoryID = data.ID
		}
		events = append(events, ev)
		next = e.ID
	}
	return events, next, nil
}

// Organization reads the current SSO and directory state of one organization.
func (c HTTPWorkOS) Organization(ctx context.Context, id string) (OrganizationState, error) {
	var out OrganizationState
	var org struct {
		ID      string   `json:"id"`
		Domains []Domain `json:"domains"`
	}
	if err := c.do(ctx, "read organization", http.MethodGet, "/organizations/"+url.PathEscape(id), "", nil, &org); err != nil {
		return out, err
	}
	out.Domains = org.Domains
	var connections struct {
		Data []struct {
			ID    string `json:"id"`
			Type  string `json:"connection_type"`
			State string `json:"state"`
		} `json:"data"`
	}
	if err := c.do(ctx, "list connections", http.MethodGet, "/connections?limit=100&organization_id="+url.QueryEscape(id), "", nil, &connections); err != nil {
		return out, err
	}
	for _, conn := range connections.Data {
		out.Connections = append(out.Connections, Connection{ID: conn.ID, Type: conn.Type, State: conn.State})
	}
	var directories struct {
		Data []Directory `json:"data"`
	}
	if err := c.do(ctx, "list directories", http.MethodGet, "/directories?limit=10&organization_id="+url.QueryEscape(id), "", nil, &directories); err != nil {
		return out, err
	}
	for _, d := range directories.Data {
		if d.State == "linked" || d.State == "active" {
			dir := d
			out.Directory = &dir
			break
		}
	}
	if out.Directory == nil {
		return out, nil
	}
	if err := c.directoryGroups(ctx, out.Directory.ID, &out); err != nil {
		return out, err
	}
	return out, c.directoryUsers(ctx, out.Directory.ID, &out)
}

func (c HTTPWorkOS) directoryGroups(ctx context.Context, directory string, out *OrganizationState) error {
	after := ""
	for {
		var page struct {
			Data         []DirectoryGroup `json:"data"`
			ListMetadata struct {
				After string `json:"after"`
			} `json:"list_metadata"`
		}
		q := url.Values{"directory": {directory}, "limit": {"100"}}
		if after != "" {
			q.Set("after", after)
		}
		if err := c.do(ctx, "list directory groups", http.MethodGet, "/directory_groups?"+q.Encode(), "", nil, &page); err != nil {
			return err
		}
		out.Groups = append(out.Groups, page.Data...)
		if page.ListMetadata.After == "" || len(out.Groups) > MaxDirectoryUsers {
			return nil
		}
		after = page.ListMetadata.After
	}
}

// directoryUsers lists the directory's users, then each group's members: WorkOS no longer fills a
// directory user's groups, so membership comes only from listing users by group.
func (c HTTPWorkOS) directoryUsers(ctx context.Context, directory string, out *OrganizationState) error {
	index := map[string]int{}
	if err := c.eachDirectoryUser(ctx, url.Values{"directory": {directory}}, func(user DirectoryUser) error {
		index[user.ID] = len(out.Users)
		out.Users = append(out.Users, user)
		if len(out.Users) > MaxDirectoryUsers {
			return ErrDirectoryTooLarge
		}
		return nil
	}); err != nil {
		return err
	}
	for _, g := range out.Groups {
		if err := c.eachDirectoryUser(ctx, url.Values{"group": {g.ID}}, func(user DirectoryUser) error {
			if i, ok := index[user.ID]; ok {
				out.Users[i].Groups = append(out.Users[i].Groups, g.ID)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// eachDirectoryUser pages through GET /directory_users with filter (directory or group).
func (c HTTPWorkOS) eachDirectoryUser(ctx context.Context, filter url.Values, visit func(DirectoryUser) error) error {
	after := ""
	for {
		var page struct {
			Data []struct {
				ID     string `json:"id"`
				Email  string `json:"email"`
				State  string `json:"state"`
				Emails []struct {
					Primary bool   `json:"primary"`
					Value   string `json:"value"`
				} `json:"emails"`
			} `json:"data"`
			ListMetadata struct {
				After string `json:"after"`
			} `json:"list_metadata"`
		}
		q := url.Values{"limit": {"100"}}
		for k, v := range filter {
			q[k] = v
		}
		if after != "" {
			q.Set("after", after)
		}
		if err := c.do(ctx, "list directory users", http.MethodGet, "/directory_users?"+q.Encode(), "", nil, &page); err != nil {
			return err
		}
		for _, u := range page.Data {
			user := DirectoryUser{ID: u.ID, Email: strings.ToLower(strings.TrimSpace(u.Email)), Active: u.State == "active"}
			for _, e := range u.Emails {
				if user.Email == "" && e.Primary {
					user.Email = strings.ToLower(strings.TrimSpace(e.Value))
				}
			}
			if err := visit(user); err != nil {
				return err
			}
		}
		if page.ListMetadata.After == "" {
			return nil
		}
		after = page.ListMetadata.After
	}
}
