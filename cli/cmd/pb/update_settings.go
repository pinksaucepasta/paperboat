package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/spf13/cobra"
)

type updateSettingsResult struct {
	AutomaticUpdates  bool       `json:"automatic_updates"`
	LocalTime         string     `json:"local_time"`
	TimeZone          string     `json:"time_zone"`
	NextMaintenanceAt *time.Time `json:"next_maintenance_at,omitempty"`
}

func updateSettingsCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "settings",
		Short: "View or change automatic update settings",
		Long: `Show or change this machine's automatic update schedule.

Official installations enable scheduled updates by default. Source and custom
installations default to availability checks only. Scheduled updates download
and verify releases ahead of the maintenance time, then install at that time
using the machine's local clock. The default maintenance time is 04:00.

Disabling automatic updates keeps availability checks enabled but leaves
downloads and installation to manual commands. Manual update check, download,
and install commands remain available either way. These settings apply to the
machine, independently of the selected account.`,
		Example: `  pb update settings
  pb update settings --auto=false
  pb update settings --auto=true --time 22:15
  pb update settings --time 04:30 --json`,
		Args:  commandArgs(cobra.NoArgs),
		RunE:  actionUpdateSettings,
	}
	command.Flags().Bool("auto", false, "enable or disable scheduled downloads and installs")
	command.Flags().String("time", "", "set the machine-local daily update time (HH:MM)")
	command.Flags().Bool("json", false, "print JSON")
	return command
}

func actionUpdateSettings(command *cobra.Command, _ []string) error {
	autoChanged := command.Flags().Changed("auto")
	timeChanged := command.Flags().Changed("time")
	auto, err := command.Flags().GetBool("auto")
	if err != nil {
		return err
	}
	localTime, err := command.Flags().GetString("time")
	if err != nil {
		return err
	}

	if timeChanged {
		preflight := autoupdate.DefaultPreferences(false)
		preflight.LocalTime = localTime
		if err := preflight.Validate(); err != nil {
			return fmt.Errorf("invalid --time %q: use 24-hour HH:MM local time", localTime)
		}
	}

	ctx, cancel := context.WithTimeout(command.Context(), 10*time.Second)
	defer cancel()
	client, err := newUpdateControlClient(10 * time.Second)
	if err != nil {
		return err
	}
	response, err := client.Settings(ctx, nil)
	if err != nil {
		return fmt.Errorf("read automatic update settings: %w", err)
	}
	if response.Settings == nil {
		return fmt.Errorf("paperboat-updated returned no automatic update settings")
	}
	settings := *response.Settings
	if err := settings.Validate(); err != nil {
		return fmt.Errorf("paperboat-updated returned invalid automatic update settings")
	}

	if autoChanged || timeChanged {
		if autoChanged {
			settings.Enabled = auto
		}
		if timeChanged {
			settings.LocalTime = localTime
		}
		if err := settings.Validate(); err != nil {
			return fmt.Errorf("invalid automatic update settings: %w", err)
		}
		response, err = client.Settings(ctx, &settings)
		if err != nil {
			return fmt.Errorf("save automatic update settings: %w", err)
		}
		if response.Settings == nil || *response.Settings != settings {
			return fmt.Errorf("paperboat-updated did not confirm the requested automatic update settings")
		}
	}

	if response.Settings == nil {
		return fmt.Errorf("paperboat-updated returned no automatic update settings")
	}
	result := updateSettingsCommandResult(*response.Settings, response.NextMaintenanceAt)
	if updateJSON(command) {
		return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{
			"schema_version": "1.0",
			"ok":             true,
			"data":           result,
		})
	}
	return writeUpdateSettingsResult(command.OutOrStdout(), result)
}

func updateSettingsCommandResult(settings autoupdate.Preferences, next time.Time) updateSettingsResult {
	now := time.Now().In(time.Local)
	zone, _ := now.Zone()
	if zone == "" {
		zone = time.Local.String()
	}
	result := updateSettingsResult{
		AutomaticUpdates: settings.Enabled,
		LocalTime:        settings.LocalTime,
		TimeZone:         zone,
	}
	if settings.Enabled && !next.IsZero() {
		localNext := next.In(time.Local)
		result.NextMaintenanceAt = &localNext
	}
	return result
}

func writeUpdateSettingsResult(output io.Writer, result updateSettingsResult) error {
	if result.AutomaticUpdates {
		if _, err := fmt.Fprintf(output, "Automatic updates: On\nDaily time: %s %s\n", result.LocalTime, result.TimeZone); err != nil {
			return err
		}
		if result.NextMaintenanceAt != nil {
			if _, err := fmt.Fprintf(output, "Next maintenance: %s\n", result.NextMaintenanceAt.Format("Mon, Jan 2 at 15:04 MST")); err != nil {
				return err
			}
		} else if _, err := fmt.Fprintln(output, "Next maintenance: unavailable"); err != nil {
			return err
		}
		_, err := fmt.Fprintln(output, "Paperboat downloads and installs updates at this time.")
		return err
	}

	if _, err := fmt.Fprintf(output, "Automatic updates: Off\nDaily time: %s %s\nNext maintenance: not scheduled\n", result.LocalTime, result.TimeZone); err != nil {
		return err
	}
	_, err := fmt.Fprintln(output, "Availability checks continue; downloads and installs wait for manual action.")
	return err
}
