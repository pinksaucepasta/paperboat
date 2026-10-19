package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/prompt"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/spf13/cobra"
)

func actionHomeAccount(command *cobra.Command) error {
	for {
		ctx := actionContext(command, nil)
		d, err := buildDeps(ctx)
		if err != nil {
			return err
		}
		items := []selector.Item{{ID: "status", Title: "Account status", Description: "Not signed in"}, {ID: "login", Title: "Sign in", Description: "Approve this CLI in your browser"}}
		if _, credentialErr := d.auth.Credential(); credentialErr == nil {
			items = []selector.Item{
				{ID: "status", Title: "Account status", Description: "Signed in"},
				{ID: "change-account", Title: "Switch account", Description: "Sign in with another account through browser approval"},
				{ID: "logout", Title: "Sign out", Description: "Revoke this CLI session"},
			}
		}
		choice, err := chooseHomeAction(command, "Account", items)
		if err != nil {
			return err
		}
		switch choice.ID {
		case "status":
			client, clientErr := backendForCommand(command)
			if clientErr != nil {
				if infoErr := showInformation(command, "Account", "Not signed in", nil); infoErr != nil && !errors.Is(infoErr, selector.ErrCanceled) {
					return infoErr
				}
				continue
			}
			var me api.Me
			meErr := homeLoading(command, "Account", "Loading account", func(ctx context.Context) error {
				var loadErr error
				me, loadErr = client.Me(ctx)
				return loadErr
			})
			if meErr != nil {
				return friendlyCommandError(meErr)
			}
			if infoErr := showInformation(command, "Account", firstNonEmpty(me.Email, me.DisplayName, me.ID), []selector.Item{{ID: "server", Title: "Paperboat server", Description: d.cfg.ServerURL}}); infoErr != nil && !errors.Is(infoErr, selector.ErrCanceled) {
				return infoErr
			}
		case "login":
			if err := executeInteractiveCommand(command, []string{"auth", "login"}); err != nil {
				if interactiveCanceled(err) {
					continue
				}
				return err
			}
		case "change-account":
			if err := executeInteractiveCommand(command, []string{"auth", "login", "--change-account"}); err != nil {
				if interactiveCanceled(err) {
					continue
				}
				return err
			}
			return selector.ErrInterrupted
		case "logout":
			if err := executeInteractiveCommand(command, []string{"auth", "logout"}); err != nil {
				if interactiveCanceled(err) {
					continue
				}
				return err
			}
		}
	}
}

func actionHomeConfig(command *cobra.Command) error {
	for {
		cfg, loadErr := config.Load(configPathFlag(command))
		if loadErr != nil {
			return loadErr
		}
		choice, err := chooseHomeAction(command, "Configuration", []selector.Item{
			{ID: "customize", Title: "Customize your CLI", Description: "Shortcuts, defaults, themes, keys, and home layout"},
			{ID: "server", Title: "Paperboat server", Description: orNone(cfg.ServerURL), Search: "edit url endpoint"},
			{ID: "auth", Title: "File credential fallback", Description: onOff(cfg.Auth.AllowFileFallback), Search: "toggle auth credentials"},
			{ID: "status-bar", Title: "Status bar", Description: cfg.StatusBar.Mode + "  ·  " + cfg.StatusBar.Theme + "  ·  fullscreen " + cfg.StatusBar.Fullscreen},
			{ID: "sync", Title: "Config sync", Description: "Enable, configure, inspect or disable sync on this machine"},
			{ID: "path", Title: "Configuration file", Description: cfg.Path()},
		})
		if err != nil {
			return err
		}
		switch choice.ID {
		case "customize":
			if err := editPreferences(command); err != nil && !interactiveCanceled(err) {
				return err
			}
		case "server":
			value, inputErr := prompt.Text(prompt.TextOptions{Title: "Paperboat server", Description: "Control-plane URL used by this CLI", Placeholder: "https://api.example.test", Initial: cfg.ServerURL, Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr(), Validate: func(value string) error {
				if strings.TrimSpace(value) == "" {
					return nil
				}
				_, normalizeErr := config.NormalizeServerURL(value)
				return normalizeErr
			}})
			if errors.Is(inputErr, prompt.ErrCanceled) {
				continue
			}
			if inputErr != nil {
				return inputErr
			}
			if strings.TrimSpace(value) == "" {
				cfg.ServerURL = ""
			} else {
				cfg.ServerURL, loadErr = config.NormalizeServerURL(value)
				if loadErr != nil {
					return loadErr
				}
			}
			if saveErr := cfg.Save(); saveErr != nil {
				return saveErr
			}
		case "auth":
			cfg.Auth.AllowFileFallback = !cfg.Auth.AllowFileFallback
			if saveErr := cfg.Save(); saveErr != nil {
				return saveErr
			}
		case "status-bar":
			if statusErr := actionHomeStatusBar(command); !errors.Is(statusErr, selector.ErrCanceled) {
				return statusErr
			}
		case "sync":
			if syncErr := actionHomeConfigSync(command); syncErr != nil && !interactiveCanceled(syncErr) {
				if err := showHomeFailure(command, syncErr); err != nil && !interactiveCanceled(err) {
					return err
				}
			}
		case "path":
			if infoErr := showInformation(command, "Configuration file", cfg.Path(), nil); infoErr != nil && !errors.Is(infoErr, selector.ErrCanceled) {
				return infoErr
			}
		}
	}
}

func actionHomeStatusBar(command *cobra.Command) error {
	for {
		cfg, err := config.Load(configPathFlag(command))
		if err != nil {
			return err
		}
		choice, err := chooseHomeAction(command, "Status bar", []selector.Item{
			{ID: "mode", Title: "Mode", Description: cfg.StatusBar.Mode},
			{ID: "fullscreen", Title: "Full-screen applications", Description: cfg.StatusBar.Fullscreen},
			{ID: "theme", Title: "Theme", Description: cfg.StatusBar.Theme},
			{ID: "privacy", Title: "Privacy", Description: onOff(cfg.StatusBar.Privacy)},
			{ID: "title", Title: "Terminal title", Description: onOff(cfg.StatusBar.TerminalTitle)},
			{ID: "reset", Title: "Restore defaults", Description: "Reset all status-bar preferences"},
		})
		if err != nil {
			return err
		}
		switch choice.ID {
		case "mode":
			cfg.StatusBar.Mode, err = chooseValue(command, "Status bar mode", cfg.StatusBar.Mode, []string{"auto", "on", "off"})
		case "fullscreen":
			cfg.StatusBar.Fullscreen, err = chooseValue(command, "Full-screen applications", cfg.StatusBar.Fullscreen, []string{"hide", "show"})
		case "theme":
			cfg.StatusBar.Theme, err = chooseValue(command, "Status bar theme", cfg.StatusBar.Theme, []string{"terminal", "dark", "light", "mono"})
		case "privacy":
			cfg.StatusBar.Privacy = !cfg.StatusBar.Privacy
		case "title":
			cfg.StatusBar.TerminalTitle = !cfg.StatusBar.TerminalTitle
		case "reset":
			cfg.StatusBar = config.DefaultStatusBarConfig()
		}
		if errors.Is(err, selector.ErrCanceled) {
			continue
		}
		if err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return err
		}
	}
}

func chooseValue(command *cobra.Command, title, current string, values []string) (string, error) {
	items := make([]selector.Item, 0, len(values))
	for _, value := range values {
		description := ""
		if value == current {
			description = "Current"
		}
		items = append(items, selector.Item{ID: value, Title: value, Description: description})
	}
	choice, err := selector.Choose(selector.Options{Title: title, Subtitle: "Choose a value", Items: items, Initial: current, Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
	return choice.ID, err
}

func onOff(value bool) string {
	if value {
		return "on"
	}
	return "off"
}

func actionHomeDoctor(command *cobra.Command) error {
	for {
		items, err := homeDoctorItems(command)
		if err != nil {
			return err
		}
		choice, err := selector.Choose(selector.Options{Title: "Diagnostics", Subtitle: "Local setup and Paperboat connectivity", Items: items, Footer: "enter view details · esc back", Context: command.Context(), Output: command.ErrOrStderr()})
		if err != nil {
			return err
		}
		if choice.ID != "config" {
			if err := showHomeText(command, choice.Title, homeDoctorDetail(choice)); err != nil {
				return err
			}
			continue
		}
		if err = actionHomeConfigSync(command); err != nil && !interactiveCanceled(err) {
			if err = showHomeFailure(command, err); err != nil && !interactiveCanceled(err) {
				return err
			}
		}
	}
}

func homeDoctorItems(command *cobra.Command) ([]selector.Item, error) {
	report := collectLocalDoctor(command.Context())
	items := []selector.Item{
		{ID: "setup", Title: "Local setup", Description: report.SetupState},
		{ID: "identity", Title: "Machine identity", Description: report.IdentityState},
		{ID: "credential", Title: "Machine credential", Description: report.CredentialState},
		{ID: "inbox", Title: "Paperboat Inbox", Description: report.InboxState + "  ·  " + report.InboxPath},
		{ID: "config", Title: "Config sync", Description: diagnosticConfigSync(report.ConfigService, nil, errors.New("assignment not checked")), Action: true},
		{ID: "runtime", Title: "Host runtime", Description: report.HostRuntime},
		{ID: "workloads", Title: "Local workloads", Description: diagnosticWorkloads(report)},
	}
	ctx := actionContext(command, nil)
	d, err := buildDeps(ctx)
	if err == nil {
		credential, credentialErr := d.auth.Credential()
		if credentialErr != nil {
			items = append(items, selector.Item{ID: "auth", Title: "Account", Description: diagnosticCredentialFailure(credentialErr)})
		} else if strings.TrimSpace(d.cfg.ServerURL) == "" {
			items = append(items, selector.Item{ID: "backend", Title: "Control plane", Description: "server not configured"})
		} else {
			var me api.Me
			meErr := homeLoading(command, "Diagnostics", "Checking control plane", func(ctx context.Context) error {
				var loadErr error
				me, loadErr = api.New(d.cfg.ServerURL, credential, nil).Me(ctx)
				return loadErr
			})
			if meErr != nil {
				items = append(items, selector.Item{ID: "backend", Title: "Control plane", Description: "unavailable  ·  " + userFacingError(meErr)})
			} else {
				items = append(items, selector.Item{ID: "auth", Title: "Account", Description: firstNonEmpty(me.Email, me.DisplayName, me.ID)}, selector.Item{ID: "backend", Title: "Control plane", Description: "authenticated"})
				if report.MachineID != "" {
					checkCtx, cancel := context.WithTimeout(command.Context(), 5*time.Second)
					assignment, assignmentErr := api.New(d.cfg.ServerURL, credential, nil).ConfigAssignment(checkCtx, report.MachineID)
					cancel()
					for index := range items {
						if items[index].ID == "config" {
							items[index].Description = diagnosticConfigSync(report.ConfigService, &assignment, assignmentErr)
						}
					}
					if assignmentErr == nil && configAssignmentEnabled(assignment) && report.ConfigService == "not_installed" {
						report.RecoveryActions = append(report.RecoveryActions, "configuration sync is assigned but its local worker is missing; reapply the configuration assignment to reinstall it")
					}
				}
			}
		}
	}
	if err != nil {
		items = append(items, selector.Item{ID: "auth", Title: "Account", Description: "saved sign-in could not be inspected"})
	}
	for index, recovery := range report.RecoveryActions {
		items = append(items, selector.Item{ID: fmt.Sprintf("recovery-%d", index), Title: "Needs attention", Description: recovery})
	}
	return items, nil
}

func showInformation(command *cobra.Command, title, subtitle string, items []selector.Item) error {
	for {
		choice, err := selector.Choose(selector.Options{Title: title, Subtitle: subtitle, Items: items, Empty: "Press Esc to go back", Footer: "enter view details  type to filter  esc back", Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
		if err != nil {
			return err
		}
		if err := showHomeText(command, choice.Title, choice.Description); err != nil && !interactiveCanceled(err) {
			return err
		}
	}
}

func chooseHomeAction(command *cobra.Command, title string, items []selector.Item) (selector.Item, error) {
	choice, err := selector.Choose(selector.Options{Title: title, Subtitle: "Choose an action", Items: items, Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
	return choice, err
}
