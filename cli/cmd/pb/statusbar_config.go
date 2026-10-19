package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/statusbar"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func statusBarConfigCommand() *cobra.Command {
	status := &cobra.Command{
		Use:   "status-bar",
		Short: "Configure the interactive terminal status bar",
		Args:  commandArgs(cobra.NoArgs),
		RunE:  func(command *cobra.Command, _ []string) error { return command.Help() },
	}
	show := &cobra.Command{Use: "show", Short: "Show the effective status-bar configuration", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error {
		cfg, err := config.Load(configPathFlag(command))
		if err != nil {
			return err
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(cfg.StatusBar)
		}
		return printStatusBarConfig(command.OutOrStdout(), cfg.StatusBar)
	}}
	show.Flags().Bool("json", false, "print JSON")
	set := &cobra.Command{Use: "set <key> <value>", Short: "Set a status-bar preference", Args: commandArgs(cobra.ExactArgs(2)), RunE: func(command *cobra.Command, args []string) error {
		cfg, err := config.Load(configPathFlag(command))
		if err != nil {
			return err
		}
		if err := setStatusBarValue(&cfg.StatusBar, args[0], args[1]); err != nil {
			return invocationError(err)
		}
		if err := cfg.Validate(); err != nil {
			return invocationError(err)
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
			return writeCLIJSON(command.OutOrStdout(), map[string]any{"path": cfg.Path(), "status_bar": cfg.StatusBar})
		}
		fmt.Fprintln(command.OutOrStdout(), cfg.Path())
		return nil
	}}
	set.Flags().Bool("json", false, "print JSON")
	reset := &cobra.Command{Use: "reset", Short: "Restore status-bar defaults", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error {
		cfg, err := config.Load(configPathFlag(command))
		if err != nil {
			return err
		}
		cfg.StatusBar = config.DefaultStatusBarConfig()
		if err := cfg.Save(); err != nil {
			return err
		}
		if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
			return writeCLIJSON(command.OutOrStdout(), map[string]any{"path": cfg.Path(), "status_bar": cfg.StatusBar})
		}
		fmt.Fprintln(command.OutOrStdout(), cfg.Path())
		return nil
	}}
	reset.Flags().Bool("json", false, "print JSON")
	preview := &cobra.Command{Use: "preview", Short: "Preview the configured status bar", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error {
		cfg, err := config.Load(configPathFlag(command))
		if err != nil {
			return err
		}
		width, _ := command.Flags().GetInt("width")
		if width == 0 {
			width = 80
			if term.IsTerminal(int(os.Stdout.Fd())) {
				if detected, _, sizeErr := term.GetSize(int(os.Stdout.Fd())); sizeErr == nil {
					width = detected
				}
			}
		}
		bar := newConfiguredStatusBar(cfg.StatusBar, statusbar.ModeOff)
		defer bar.Close()
		bar.SetIdentity("paperboat", "default")
		bar.SetUsage("100", "12 GB")
		bar.SetConfigSync("healthy")
		bar.SetConnection("connected")
		fmt.Fprintln(command.OutOrStdout(), bar.Render(width))
		bar.SetConnection("reconnecting")
		bar.Loading("Reconnecting")
		fmt.Fprintln(command.OutOrStdout(), bar.Render(width))
		bar.SetConnection("failed")
		bar.FailureFor("preview", "Connection lost")
		fmt.Fprintln(command.OutOrStdout(), bar.Render(width))
		return nil
	}}
	preview.Flags().Int("width", 0, "preview width (20-500 columns)")
	preview.PreRunE = func(command *cobra.Command, _ []string) error {
		width, _ := command.Flags().GetInt("width")
		if width != 0 && (width < 20 || width > 500) {
			return invocationError(errors.New("--width must be between 20 and 500"))
		}
		return nil
	}
	status.AddCommand(show, set, reset, preview)
	return status
}

func configPathFlag(command *cobra.Command) string {
	value, _ := command.Flags().GetString("config")
	return value
}

func printStatusBarConfig(writer io.Writer, value config.StatusBarConfig) error {
	_, err := fmt.Fprintf(writer, "mode: %s\nfullscreen: %s\ntheme: %s\nprivacy: %t\nterminal_title: %t\nnotice_seconds: %d\nleft: %s\ncenter: %s\nright: %s\nforeground: %s\nbackground: %s\naccent: %s\nwarning: %s\nerror: %s\n",
		value.Mode, value.Fullscreen, value.Theme, value.Privacy, value.TerminalTitle, value.NoticeSeconds,
		formatWidgetList(value.Left), formatWidgetList(value.Center), formatWidgetList(value.Right),
		inheritedColor(value.Colors.Foreground), inheritedColor(value.Colors.Background), inheritedColor(value.Colors.Accent), inheritedColor(value.Colors.Warning), inheritedColor(value.Colors.Error))
	return err
}

func inheritedColor(value string) string {
	if value == "" {
		return "inherit"
	}
	return value
}

func formatWidgetList(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ",")
}

func setStatusBarValue(value *config.StatusBarConfig, key, raw string) error {
	key = strings.ToLower(strings.TrimSpace(key))
	raw = strings.TrimSpace(raw)
	switch key {
	case "mode":
		value.Mode = strings.ToLower(raw)
	case "fullscreen":
		value.Fullscreen = strings.ToLower(raw)
	case "theme":
		value.Theme = strings.ToLower(raw)
	case "privacy", "terminal-title":
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("%s must be true or false", key)
		}
		if key == "privacy" {
			value.Privacy = parsed
		} else {
			value.TerminalTitle = parsed
		}
	case "notice-seconds":
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 60 {
			return localArgumentError("notice-seconds must be between 1 and 60")
		}
		value.NoticeSeconds = parsed
	case "left", "center", "right":
		widgets := parseWidgetList(raw)
		switch key {
		case "left":
			value.Left = widgets
		case "center":
			value.Center = widgets
		case "right":
			value.Right = widgets
		}
	case "foreground", "background", "accent", "warning", "error":
		switch key {
		case "foreground":
			value.Colors.Foreground = raw
		case "background":
			value.Colors.Background = raw
		case "accent":
			value.Colors.Accent = raw
		case "warning":
			value.Colors.Warning = raw
		case "error":
			value.Colors.Error = raw
		}
	default:
		return fmt.Errorf("unknown status-bar key %q", key)
	}
	return nil
}

func parseWidgetList(raw string) []string {
	if raw == "" || strings.EqualFold(raw, "none") {
		return []string{}
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		result = append(result, strings.ToLower(strings.TrimSpace(part)))
	}
	return result
}

func newConfiguredStatusBar(value config.StatusBarConfig, mode string) *statusbar.Bar {
	return statusbar.New(statusbar.Options{
		Mode: mode, Fullscreen: value.Fullscreen, Theme: value.Theme, Privacy: value.Privacy, TerminalTitle: false,
		Colors:         statusbar.Colors{Foreground: value.Colors.Foreground, Background: value.Colors.Background, Accent: value.Colors.Accent, Warning: value.Colors.Warning, Error: value.Colors.Error},
		NoticeDuration: time.Duration(value.NoticeSeconds) * time.Second,
		Layout:         statusbar.Layout{Left: value.Left, Center: value.Center, Right: value.Right},
	})
}
