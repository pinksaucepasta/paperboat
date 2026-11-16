package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
	clientapi "github.com/pinksaucepasta/paperboat/shared/client/api"
	"github.com/pinksaucepasta/paperboat/shared/recipe"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

// recipeTokenFromPair returns the recipe token `pb pair` was given: the argument, else PB_RECIPE
// when no enrollment token was given. ok is false when pairing is by enrollment token.
func recipeTokenFromPair(args []string, enrollmentGiven bool) (token string, ok bool, err error) {
	if len(args) == 1 {
		if enrollmentGiven {
			return "", false, localArgumentError("give a recipe token or an enrollment token, not both")
		}
		return args[0], true, nil
	}
	if v := strings.TrimSpace(os.Getenv("PB_RECIPE")); v != "" && !enrollmentGiven {
		return v, true, nil
	}
	return "", false, nil
}

// recipePair pairs this computer from a recipe (PLAN F26): the recipe token starts a session of the
// recipe's creator limited to this machine, then the normal `pb setup` flow adds the machine, which
// the server makes the recipe's. Nothing prompts. A machine already paired and signed in is left as it
// is, so running the same install again does nothing.
func recipePair(c *command.Context, tokenText string, setupArgs []string) error {
	token, err := recipe.ParseToken(tokenText)
	if err != nil {
		return localArgumentError(err.Error())
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
	store, err := loginProfileStore(cfg, issuer)
	if err != nil {
		return err
	}
	// A machine identity that cannot be read is not "paired": the preflight below then names the missing
	// administrator rights, which is why it is unreadable for a standard Windows user (victus, rcpwn).
	machineID, identityErr := loginLocalMachineID(cfg)
	if _, loadErr := store.Load(issuer); identityErr == nil && machineID != "" && loadErr == nil {
		// A machine removed since (recipe revoked, lifetime, offline) is not "paired": say how to start over.
		// The session source refreshes an expired access token, so only a rejected session counts.
		if source, err := auth.NewSource(cfg); err == nil {
			if credential, err := source.Credential(); err == nil {
				client := api.New(issuer, credential, loginHTTPClient())
				client.SetCredentialSource(source)
				if _, err := client.Me(c.Context); errors.Is(err, api.ErrUnauthenticated) {
					return usageError{err: err, publicMessage: recipeMachineRemovedMessage, refusal: true}
				}
			} else if errors.Is(err, api.ErrUnauthenticated) {
				return usageError{err: err, publicMessage: recipeMachineRemovedMessage, refusal: true}
			}
		}
		_, err = fmt.Fprintln(c.Writer, "This machine is already paired; nothing to do.")
		return err
	}
	// The install steps read PB_RECIPE to never prompt; `pb pair pbr_…` gives the token as an argument.
	if err := os.Setenv("PB_RECIPE", tokenText); err != nil {
		return err
	}
	if err := recipeInstallPreflight(c.Context); err != nil {
		return usageError{err: err, publicMessage: hostruntimecmd.RecipeNeedsRootMessage, refusal: true}
	}
	if identityErr != nil {
		return identityErr
	}
	label := "Recipe on " + firstNonEmpty(loginDeviceName(), runtime.GOOS)
	started, err := recipeSessionStart(c, issuer, token, label)
	if err != nil {
		return fmt.Errorf("start pairing from the recipe: %w", friendlyCommandError(err))
	}
	t := started.TokenSet
	if t.TokenType != "Bearer" || t.AccessToken == "" || t.RefreshToken == "" || t.CLIClientSessionID == "" || t.ExpiresIn <= 0 || t.ExpiresIn > 86400 {
		return errors.New("the server returned an invalid session for the recipe")
	}
	credential := config.Credential{AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, TokenType: t.TokenType, ExpiresAt: serverclock.Now().UTC().Add(time.Duration(t.ExpiresIn) * time.Second)}
	client := api.New(issuer, credential, loginHTTPClient())
	me, err := client.Me(c.Context)
	if err != nil {
		return fmt.Errorf("check the recipe's account: %w", friendlyCommandError(err))
	}
	previous := ""
	if old, e := store.Load(issuer); e == nil {
		previous = old.CLIClientSessionID
	} else if !errors.Is(e, config.ErrNoCredentials) {
		return e
	}
	if _, err = auth.CompleteLogin(c.Context, auth.LoginCompletion{Store: store, Client: client, Issuer: issuer, Credential: credential, SessionID: t.CLIClientSessionID, ExpectedAccountID: me.ID, PreviousSessionID: &previous, AllowAccountChange: true, ServerAuthorized: true}); err != nil {
		return fmt.Errorf("save the recipe session; run the same pb pair again: %w", err)
	}
	if err = cfg.Save(); err != nil {
		return fmt.Errorf("save the configuration; run the same pb pair again: %w", err)
	}
	// The recipe key opens the vaults the recipe reads.
	if err = saveRecipeKey(cfg, token); err != nil {
		return err
	}
	if !c.Bool("json") {
		if _, err = fmt.Fprintf(c.ErrWriter, "Pairing this machine from recipe %s.\n", started.Recipe.Name); err != nil {
			return err
		}
	}
	args := []string{"setup"}
	if path := strings.TrimSpace(c.String("config")); path != "" {
		args = append([]string{"--config", path}, args...)
	}
	if code := runRecipeSetup(c, append(args, setupArgs...)); code != 0 {
		return exitCodeError{code: code}
	}
	if recipeConfigNeedsService && recipeHasConfig(started.Recipe.Recipe) {
		// The daemon applies [config] unelevated; on Windows the config service needs an administrator
		// once, and pairing already runs elevated (recipeInstallPreflight).
		machineID, err := loginLocalMachineID(cfg)
		if err == nil {
			err = recipeConfigService(c.Context, machineID, true)
		}
		if err != nil {
			return fmt.Errorf("install the config sync service; run pb config sync apply in an administrator PowerShell: %w", err)
		}
	}
	return nil
}

// recipeConfigNeedsService and recipeConfigService are replaced by tests.
var (
	recipeConfigNeedsService = runtime.GOOS == "windows"
	recipeConfigService      = manageConfigService
)

func recipeHasConfig(spec json.RawMessage) bool {
	var r struct {
		Config struct {
			Repository string `json:"repository"`
		} `json:"config"`
	}
	return json.Unmarshal(spec, &r) == nil && r.Config.Repository != ""
}

const recipeMachineRemovedMessage = "This machine was removed from Paperboat (its recipe was revoked, or machine_lifetime or remove_after_offline removed it). Run `pb uninstall`, then the recipe install again."

// recipeSessionStart, recipeInstallPreflight and runRecipeSetup are replaced by tests.
var recipeInstallPreflight = hostruntimecmd.RecipeInstallPreflight
var recipeSessionStart = func(c *command.Context, issuer string, token recipe.Token, label string) (clientapi.RecipeSession, error) {
	return api.StartRecipeSession(c.Context, issuer, token, label, recipeDeviceType(), runtime.GOOS, loginHTTPClient())
}

var runRecipeSetup func(c *command.Context, args []string) int

// Set in init: run builds the command tree, which refers back to recipePair.
func init() {
	runRecipeSetup = func(c *command.Context, args []string) int {
		return run(c.Context, args, c.Writer, c.ErrWriter)
	}
}

func recipeDeviceType() string {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "container"
	}
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return "server"
	}
	return "desktop"
}
