package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/spf13/cobra"
)

func tunnelLogsCommand() *cobra.Command {
	command := tunnelCommand("logs <tunnel>", "Show tunnel logs", cobra.ExactArgs(1), runTunnelLogs)
	command.Flags().String("cursor", "", "continue after a log cursor")
	command.Flags().Int("limit", 100, "maximum entries per request (1-200)")
	command.Flags().Bool("follow", false, "wait for new log entries")
	command.Flags().Duration("interval", time.Second, "follow polling interval (250ms-1m)")
	tunnelJSONFlag(command)
	return command
}

func runTunnelLogs(command *cobra.Command, args []string) error {
	cursor, _ := command.Flags().GetString("cursor")
	limit, _ := command.Flags().GetInt("limit")
	follow, _ := command.Flags().GetBool("follow")
	interval, _ := command.Flags().GetDuration("interval")
	if limit < 1 || limit > 200 {
		return errors.New("limit must be between 1 and 200")
	}
	if interval < 250*time.Millisecond || interval > time.Minute {
		return errors.New("interval must be between 250ms and 1m")
	}
	if len(cursor) > 4096 || strings.ContainsAny(cursor, "\x00\r\n") {
		return errors.New("cursor is invalid")
	}
	client, ctx, err := tunnelClient(command)
	if err != nil {
		return err
	}
	tunnelID, err := resolveTunnelSelectorForCommand(ctx, client, args[0])
	if err != nil {
		return err
	}
	for {
		page, requestErr := retryTunnelRead(ctx, func() (api.TunnelLogPage, error) {
			return client.ListTunnelLogsV1(ctx, tunnelID, cursor, limit)
		})
		if requestErr != nil {
			return requestErr
		}
		if err := writeTunnelLogs(command, page, follow); err != nil {
			return err
		}
		previousCursor := cursor
		if page.NextCursor != "" {
			cursor = page.NextCursor
		} else if len(page.Items) > 0 {
			cursor = page.Items[len(page.Items)-1].Cursor
		}
		if !follow {
			jsonOutput, _ := command.Flags().GetBool("json")
			if !jsonOutput {
				return writeInventoryContinuation(command, args, page.NextCursor, limit)
			}
			return nil
		}
		if cursor != "" && cursor == previousCursor && len(page.Items) > 0 {
			return api.ErrUnsafeTunnelResponse
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func writeTunnelLogs(command *cobra.Command, page api.TunnelLogPage, follow bool) error {
	jsonOutput, _ := command.Flags().GetBool("json")
	writer := command.OutOrStdout()
	if jsonOutput && !follow {
		return json.NewEncoder(writer).Encode(page)
	}
	encoder := json.NewEncoder(writer)
	for _, entry := range page.Items {
		if jsonOutput {
			if err := encoder.Encode(entry); err != nil {
				return err
			}
			continue
		}
		message := strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(entry.Message)
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", entry.OccurredAt.UTC().Format(time.RFC3339), entry.Level, entry.Component, entry.Code, message); err != nil {
			return err
		}
	}
	return nil
}
