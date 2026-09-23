package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

// EnrollmentLogin exchanges an existing enrollment grant for a CLI session only.
type EnrollmentLogin struct {
	TokenSet
	Account config.Account `json:"account"`
}

func (c *Client) LoginWithEnrollmentToken(ctx context.Context, token, verifier, label, platform string) (EnrollmentLogin, error) {
	var out EnrollmentLogin
	client := *c.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	err := publicCall(ctx, c.baseURL, "/v1/auth/enrollment/token", map[string]string{
		"enrollment_token": token, "verifier": verifier, "client_label": label, "os": platform,
	}, "", &out, &client)
	if err != nil {
		return EnrollmentLogin{}, err
	}
	if out.AccessToken == "" || out.RefreshToken == "" || out.CLIClientSessionID == "" || out.TokenType != "Bearer" || out.ExpiresIn <= 0 || out.Account.ID == "" {
		return EnrollmentLogin{}, errors.New("server returned an invalid enrollment session")
	}
	return out, nil
}
