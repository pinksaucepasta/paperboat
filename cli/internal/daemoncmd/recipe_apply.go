package daemoncmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	sessionauth "github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
	"github.com/pinksaucepasta/paperboat/internal/processlaunch"
	"github.com/pinksaucepasta/paperboat/internal/recipekey"
	"github.com/pinksaucepasta/paperboat/shared/backoff"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"github.com/pinksaucepasta/paperboat/shared/recipe"
)

// A machine paired from a recipe (PLAN F26) applies its recipe's sections by running this pb's own
// commands (pb preview, pb tunnel create, pb config sync apply, pb env hook), each an "ensure": what
// exists and matches is left alone, what is missing is created, what the recipe no longer lists is
// removed. It re-reads GET /v1/recipe-machine when the server moves the machine's "recipe" topic, on
// start, and otherwise rarely; a failed section is retried with backoff (15 s doubling to 30 min).
const (
	recipeIdleRefresh = 30 * time.Minute
	// The first apply can race the host runtime starting after pairing (its preview owner files appear
	// seconds later), so the first retry comes soon.
	recipeFirstRetry   = 15 * time.Second
	recipeMaximumRetry = 30 * time.Minute
	recipeStepTimeout  = 10 * time.Minute
	recipeRunTimeout   = 3 * time.Minute
)

// recipeApplyState is what earlier applies created, so re-applying is a no-op and removed sections
// can be undone. Keys of Previews and Tunnels are the section's canonical JSON; values are the names
// pb printed. An expired preview stays listed, so it is never started again.
type recipeApplyState struct {
	Revision int64             `json:"revision"`
	Previews map[string]string `json:"previews"`
	Tunnels  map[string]string `json:"tunnels"`
	Config   string            `json:"config"`
	Hook     bool              `json:"hook"`
}

// recipeRunner runs one pb command and returns its stdout.
type recipeRunner func(ctx context.Context, args ...string) ([]byte, error)

type recipeApplier struct {
	statePath string
	machine   string // tunnel name when the recipe gives none
	run       recipeRunner
}

func pbRunner(configPath string) recipeRunner {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		self, err := os.Executable()
		if err != nil {
			return nil, err
		}
		if configPath != "" {
			args = append([]string{"--config", configPath}, args...)
		}
		ctx, cancel := context.WithTimeout(ctx, recipeRunTimeout)
		defer cancel()
		var stdout, stderr bytes.Buffer
		command := exec.CommandContext(ctx, self, args...)
		processlaunch.ConfigureBackground(command)
		command.Stdout, command.Stderr = &stdout, &stderr
		err = command.Run()
		return stdout.Bytes(), recipeCommandError(args, stdout.Bytes(), stderr.Bytes(), err)
	}
}

// recipeCommandError names the pb command and pb's own one-line reason (never a secret).
func recipeCommandError(args []string, stdout, stderr []byte, err error) error {
	if err == nil {
		return nil
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	reason := ""
	if json.Unmarshal(stdout, &envelope) == nil {
		reason = envelope.Error.Message
	}
	if reason == "" {
		lines := strings.Split(strings.TrimSpace(string(stderr)), "\n")
		reason = strings.TrimSpace(lines[len(lines)-1])
	}
	name := strings.Join(args, " ")
	if i := strings.Index(name, " --"); i > 0 {
		name = name[:i]
	}
	if reason == "" {
		return fmt.Errorf("pb %s: %w", name, err)
	}
	return fmt.Errorf("pb %s: %s", name, reason)
}

// jsonData returns the object of a pb --json result: its "data" envelope, or the bare resource that
// pb preview and pb tunnel print.
func jsonData(out []byte) map[string]any {
	var object map[string]any
	_ = json.Unmarshal(out, &object)
	if data, ok := object["data"].(map[string]any); ok {
		return data
	}
	return object
}

func (a *recipeApplier) load() recipeApplyState {
	state := recipeApplyState{}
	if data, err := os.ReadFile(a.statePath); err == nil {
		_ = json.Unmarshal(data, &state)
	}
	if state.Previews == nil {
		state.Previews = map[string]string{}
	}
	if state.Tunnels == nil {
		state.Tunnels = map[string]string{}
	}
	return state
}

func (a *recipeApplier) save(state recipeApplyState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	temporary := a.statePath + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, a.statePath)
}

// apply ensures every section of r and returns each section's outcome for the server.
func (a *recipeApplier) apply(ctx context.Context, revision int64, r recipe.Recipe) (map[string]any, error) {
	state := a.load()
	sections := map[string]any{}
	var failed []error
	note := func(section string, err error) {
		if err != nil {
			sections[section] = map[string]any{"error": err.Error()}
			failed = append(failed, err)
			return
		}
		sections[section] = map[string]any{"ok": true}
	}
	if len(r.Preview) > 0 || len(state.Previews) > 0 {
		note("preview", a.ensure(ctx, state.Previews, previewSpecs(r.Preview), "preview", func(name string) []string { return []string{"preview", "stop", name, "--json"} }))
	}
	if len(r.Tunnel) > 0 || len(state.Tunnels) > 0 {
		note("tunnel", a.ensure(ctx, state.Tunnels, tunnelSpecs(r.Tunnel, a.machine), "tunnel", func(name string) []string { return []string{"tunnel", "delete", name, "--json"} }))
	}
	if r.Config != nil && state.Config != r.Config.Repository {
		_, err := a.confirmed(ctx, "config", "sync", "apply", "--repository", r.Config.Repository, "--json")
		if err == nil {
			state.Config = r.Config.Repository
		}
		note("config", err)
	}
	if r.Env != nil && r.Env.Shell && !state.Hook {
		_, err := a.run(ctx, "env", "hook", "--install")
		state.Hook = err == nil
		note("env", err)
	}
	state.Revision = revision
	return sections, errors.Join(append(failed, a.save(state))...)
}

// ensure creates each wanted spec that earlier applies did not, and removes what they created that is
// no longer wanted. created maps spec keys to names and is updated in place.
func (a *recipeApplier) ensure(ctx context.Context, created map[string]string, wanted map[string][]string, kind string, remove func(string) []string) error {
	var failed []error
	for key, args := range wanted {
		if _, ok := created[key]; ok {
			continue
		}
		out, err := a.run(ctx, args...)
		if err != nil {
			failed = append(failed, err)
			continue
		}
		data := jsonData(out)
		// pb tunnel create --json nests the resource under its kind: {"kind":"tunnel_create","tunnel":{...}}.
		if nested, ok := data[kind].(map[string]any); ok {
			data = nested
		}
		name, _ := data["name"].(string)
		if name == "" {
			name, _ = data["id"].(string)
		}
		if name == "" {
			failed = append(failed, fmt.Errorf("pb %s printed no %s name", kind, kind))
			continue
		}
		created[key] = name
	}
	for key, name := range created {
		if _, ok := wanted[key]; ok {
			continue
		}
		if _, err := a.confirmed(ctx, remove(name)...); err != nil && !notFound(err) {
			failed = append(failed, err)
			continue
		}
		delete(created, key)
	}
	return errors.Join(failed...)
}

// confirmed runs a command that may ask for a confirmation code and runs it again with the code: the
// recipe's creation is the approval.
func (a *recipeApplier) confirmed(ctx context.Context, args ...string) ([]byte, error) {
	out, err := a.run(ctx, args...)
	if err == nil {
		return out, nil
	}
	data := jsonData(out)
	if data["outcome"] != "confirmation_required" {
		return out, err
	}
	token, _ := data["confirmation_token"].(string)
	return a.run(ctx, append(args, "--confirm", token)...)
}

func notFound(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "not found") || strings.Contains(text, "no such")
}

func specKey(kind string, v any) string {
	b, _ := json.Marshal(v)
	return kind + ":" + string(b)
}

func previewSpecs(previews []recipe.Preview) map[string][]string {
	out := map[string][]string{}
	for _, p := range previews {
		args := []string{"preview", strconv.Itoa(p.Port), "--background", "--ttl", recipeDuration(p.TTL), "--json"}
		if p.Private {
			args = append(args, "--private")
		}
		if p.Team {
			args = append(args, "--team")
		}
		if p.HostHeader != "" {
			args = append(args, "--host-header", p.HostHeader)
		}
		if p.Domain != "" {
			args = append(args, "--domain", p.Domain)
		}
		out[specKey("preview", p)] = args
	}
	return out
}

func tunnelSpecs(tunnels []recipe.Tunnel, machine string) map[string][]string {
	out := map[string][]string{}
	for _, t := range tunnels {
		args := []string{"tunnel", "create", firstNonEmptyString(t.Name, machine), "--json"}
		if t.Port != 0 {
			args = append(args, "--port", strconv.Itoa(t.Port))
		} else {
			args = append(args, "--from", t.From)
		}
		if t.Private {
			args = append(args, "--private")
		}
		if t.Team {
			args = append(args, "--team")
		}
		if t.Domain != "" {
			args = append(args, "--domain", t.Domain)
		}
		if t.Duration != "" {
			args = append(args, "--duration", recipeDuration(t.Duration))
		}
		out[specKey("tunnel", t)] = args
	}
	return out
}

// recipeDuration turns a recipe duration ("2h", "7d") into one pb's flags read.
func recipeDuration(s string) string {
	d, ok := recipe.ParseDuration(s)
	if !ok {
		return s
	}
	return d.String()
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

type recipeMachineFetch func(context.Context) (api.RecipeMachineState, error)
type recipeApplyReport func(context.Context, int64, map[string]any, []byte) error

func recipeClients(serverURL string, auth *sessionauth.Source, transport http.RoundTripper) (recipeMachineFetch, recipeApplyReport) {
	client := func(ctx context.Context) (*api.Client, error) {
		credential, err := auth.WithContext(ctx).Credential()
		if err != nil {
			return nil, err
		}
		return api.New(serverURL, credential, &http.Client{Transport: transport}), nil
	}
	fetch := func(ctx context.Context) (api.RecipeMachineState, error) {
		c, err := client(ctx)
		if err != nil {
			return api.RecipeMachineState{}, err
		}
		return c.RecipeMachine(ctx)
	}
	report := func(ctx context.Context, revision int64, sections map[string]any, machinePublic []byte) error {
		c, err := client(ctx)
		if err != nil {
			return err
		}
		return c.ApplyRecipe(ctx, revision, sections, machinePublic)
	}
	return fetch, report
}

// refreshRecipe applies the recipe of a machine paired from one (it runs only where pb pair left a
// recipe key file). A failing apply is the recipe step, shown by pb doctor until it succeeds.
func refreshRecipe(ctx context.Context, steps *health.Registry, applier *recipeApplier, configPath string, fetch recipeMachineFetch, report recipeApplyReport, changed <-chan struct{}) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-changed:
			stopTimer(timer)
		}
		stepCtx, cancel := context.WithTimeout(ctx, recipeStepTimeout)
		// Only a recipe machine runs this loop (main.go), so a refusal here is a failure to show and
		// retry, never a reason to stop silently.
		state, err := fetch(stepCtx)
		if err == nil {
			err = applyRecipeState(stepCtx, applier, configPath, state, report)
		}
		err = recipeStepError(err)
		cancel()
		wait := recipeIdleRefresh
		if err == nil {
			failures = 0
			steps.Set(localdaemon.StepRecipe, health.Ready, "", 0)
		} else if ctx.Err() == nil {
			failures++
			wait = backoff.Retry(failures, recipeFirstRetry, recipeMaximumRetry)
			steps.Fail(localdaemon.StepRecipe, err, wait)
			diagnosticlog.TryWarn("could not apply this machine's recipe; the next attempt follows", "error", err)
		}
		timer.Reset(wait)
	}
}

func recipeStatePath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "recipe-apply.json")
}

// applyRecipeState takes a rotated recipe key sealed to this machine, applies the sections and
// reports them with the machine's own public key.
func applyRecipeState(ctx context.Context, applier *recipeApplier, configPath string, state api.RecipeMachineState, report recipeApplyReport) error {
	key, ok, err := recipekey.Load(configPath)
	if err != nil || !ok {
		return errors.Join(err, errors.New("this machine's recipe key file is missing; run the recipe install again"))
	}
	if len(state.MachineKey) > 0 {
		if key, _, err = recipekey.Rotated(configPath, key, state.MachineKey); err != nil {
			return err
		}
	}
	public, err := key.MachinePublic()
	if err != nil {
		return err
	}
	var r recipe.Recipe
	if err := json.Unmarshal(state.Recipe, &r); err != nil {
		return fmt.Errorf("read the recipe: %w", err)
	}
	sections, err := applier.apply(ctx, state.Revision, r)
	return errors.Join(err, report(ctx, state.Revision, sections, public))
}

// recipeStepError names a rejected recipe session in the recipe's terms: its credential comes from the
// recipe install, not pb login.
func recipeStepError(err error) error {
	if errors.Is(err, api.ErrUnauthenticated) {
		return fmt.Errorf("the server rejected this machine's recipe session (the recipe was revoked or rotated without this machine, or the machine was removed); run the recipe install again: %w", err)
	}
	return err
}
