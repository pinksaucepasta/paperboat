package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

// apiTokenLogin signs this computer in with an API token instead of browser approval (PLAN
// F18-06): the server turns the token into a CLI session of its creator that carries the token,
// so pb works as usual within the token's scope. The token is read from a file or stdin and is
// never saved; the session ends when the token is revoked or expires.
func apiTokenLogin(c *command.Context, path string) error {
	token, err := readAPIToken(c, path)
	if err != nil {
		return err
	}
	cfg, err := config.Load(c.String("config"))
	if err != nil {
		return err
	}
	issuer := cfg.ServerURL
	if v := strings.TrimSpace(c.String("server")); v != "" {
		issuer = v
	}
	if issuer, err = config.NormalizeServerURL(issuer); err != nil {
		return invocationError(err)
	}
	cfg.ServerURL = issuer
	store, err := config.ProfileStoreFor(cfg)
	if err != nil {
		return err
	}
	if err = store.Recover(issuer); err != nil {
		return err
	}
	label := "API token on " + firstNonEmpty(loginDeviceName(), runtime.GOOS)
	t, err := api.StartAPITokenSession(c.Context, issuer, token, label, apiTokenDeviceType(), runtime.GOOS, loginHTTPClient())
	if err != nil {
		return fmt.Errorf("sign in with the API token: %w", friendlyCommandError(err))
	}
	if t.TokenType != "Bearer" || t.AccessToken == "" || t.RefreshToken == "" || t.CLIClientSessionID == "" || t.ExpiresIn <= 0 || t.ExpiresIn > 86400 {
		return errors.New("the server returned an invalid session for the API token")
	}
	credential := config.Credential{AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, TokenType: t.TokenType, ExpiresAt: time.Now().UTC().Add(time.Duration(t.ExpiresIn) * time.Second)}
	client := api.New(issuer, credential, loginHTTPClient())
	me, err := client.Me(c.Context)
	if err != nil {
		return fmt.Errorf("check the API token's account: %w", friendlyCommandError(err))
	}
	if me.ID == "" || me.Status != "active" {
		return errors.New("the API token's account is not active")
	}
	previous := ""
	if old, e := store.Load(issuer); e == nil {
		previous = old.CLIClientSessionID
	} else if !errors.Is(e, config.ErrNoCredentials) {
		return e
	}
	if _, err = auth.CompleteLogin(c.Context, auth.LoginCompletion{Store: store, Client: client, Issuer: issuer, Credential: credential, SessionID: t.CLIClientSessionID, ExpectedAccountID: me.ID, PreviousSessionID: &previous, AllowAccountChange: true}); err != nil {
		return fmt.Errorf("activate the API token session; retry `pb login --api-token-file`: %w", err)
	}
	if err = cfg.Save(); err != nil {
		return fmt.Errorf("signed in; retry `pb login --api-token-file` to finish configuration: %w", err)
	}
	if err = refreshSignedInDaemon(c.Context, cfg); err != nil {
		return browserLoginFailure{message: "Signed in with the API token, but the installed daemon could not refresh. Run the same pb login again, or pb doctor to check recovery.", cause: err}
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, map[string]any{"signed_in": true, "with_api_token": true, "account": config.Account{ID: me.ID, Email: me.Email, DisplayName: me.DisplayName}})
	}
	_, err = fmt.Fprintf(c.Writer, "Signed in as %s with an API token. This computer can do only what the token allows.\n", firstNonEmpty(me.Email, me.DisplayName, me.ID))
	return err
}

func readAPIToken(c *command.Context, path string) (string, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(io.LimitReader(os.Stdin, 4096))
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("read the API token: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if !strings.HasPrefix(token, "pbat_") || strings.ContainsAny(token, " \t\r\n") || len(token) > 256 {
		return "", localArgumentError("the API token file must hold one API token (pbat_…), as printed by pb api-token create")
	}
	return token, nil
}

func apiTokenDeviceType() string {
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return "server"
	}
	return "desktop"
}
