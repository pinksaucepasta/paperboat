package main

import (
	"context"
	"io"
	"net/http"
	"runtime"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/command"
	"github.com/pinksaucepasta/paperboat/cli/internal/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/cli/internal/selector"
)

// authDependencies are the edges of sign-in, pairing from a recipe and pairing a machine that tests replace: the
// credential store, the login prompt and poll wait, the HTTP client, this computer's identity and installed service,
// and the pairing runtime. Production uses the real ones (defaultAuthDependencies).
type authDependencies struct {
	credentialStoreAvailable func() bool
	chooseLoginAction        func(selector.Options) (selector.Item, error)
	loginWait                func(ctx context.Context, delay time.Duration) error
	httpClient               func() *http.Client
	localMachineID           func(*config.Config) (string, error)
	pairLoginDevice          func(c *command.Context, server string, enrollment *config.LoginEnrollment, name string) error
	daemonAwaitingEnrollment func(context.Context) bool
	serviceProfile           func() (string, error) // the config the installed local daemon service runs for
	recipeConfigNeedsService bool
	recipeConfigService      func(ctx context.Context, machineID string, install bool) error
	recipeInstallPreflight   func(context.Context) error
	runRecipeSetup           func(c *command.Context, args []string) int
	pairBootstrap            func(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int // returns 0 only once the new machine is verified (daemon bound)
}

func defaultAuthDependencies() authDependencies {
	return authDependencies{
		credentialStoreAvailable: config.CredentialStoreAvailable,
		chooseLoginAction:        selector.Choose,
		loginWait:                waitForLoginPoll,
		httpClient:               newLoginHTTPClient,
		localMachineID:           localMachineID,
		pairLoginDevice:          pairLoginEnrollment,
		daemonAwaitingEnrollment: localDaemonAwaitingEnrollment,
		serviceProfile:           installedServiceProfile,
		recipeConfigNeedsService: runtime.GOOS == "windows",
		recipeConfigService:      manageConfigService,
		recipeInstallPreflight:   hostruntimecmd.RecipeInstallPreflight,
		runRecipeSetup: func(c *command.Context, args []string) int {
			return run(c.Context, args, c.Writer, c.ErrWriter)
		},
		pairBootstrap: hostruntimecmd.Execute,
	}
}

func authDependenciesOf(ctx context.Context) authDependencies {
	return dependenciesIn(ctx, defaultAuthDependencies)
}
