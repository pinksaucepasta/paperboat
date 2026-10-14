package main

import (
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/prompt"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/spf13/cobra"
	"os"
	"strconv"
)

func actionHomeSentFiles(command *cobra.Command) error {
	backend, err := backendForCommand(command)
	if err != nil {
		return err
	}
	for {
		machines, err := backend.ListUserMachines(command.Context())
		if err != nil {
			return err
		}
		items := []selector.Item{}
		source, _ := configuredMachineID()
		for _, machine := range machines {
			if machine.Capabilities.FileReceive.Configured && machine.ID != source {
				items = append(items, selector.Item{ID: machine.ID, Title: machine.Alias, Description: machineStatusSummary(machine)})
			}
		}
		items = append(items, selector.Item{ID: "commands", Title: "Transfer commands", Description: "Session destinations, send files and other options", Action: true})
		choice, err := chooseHomeAction(command, "Sent files — choose destination", items)
		if err != nil {
			return err
		}
		if choice.ID == "commands" {
			err = actionHomeCommands(command, []string{"send"})
		} else {
			err = actionHomeTransferHistory(command, choice.ID, choice.Title)
		}
		if err != nil && !interactiveCanceled(err) {
			if err := showHomeFailure(command, err); err != nil {
				return err
			}
		}
	}
}
func actionHomeTransferHistory(parent *cobra.Command, machineID, title string) error {
	// Reuse the CLI's native authorization and lease ownership for discovery.
	root := newRootCommand()
	root.SetContext(parent.Context())
	for _, name := range []string{"config", "server", "workspace"} {
		if flag := parent.Flags().Lookup(name); flag != nil && flag.Value.String() != "" {
			if err := root.PersistentFlags().Set(name, flag.Value.String()); err != nil {
				return err
			}
		}
	}
	command, _, err := root.Find([]string{"send", "list"})
	if err != nil {
		return err
	}
	// Initialize inherited flags exactly as Cobra does before a normal invocation.
	command.Flags().AddFlagSet(root.PersistentFlags())
	if err := command.Flags().Set("on", machineID); err != nil {
		return err
	}
	client, _, lease, err := transferClientForCommand(command, nil)
	if err != nil {
		return err
	}
	defer lease.Close()
	defer client.Close()
	offset := 0
	history := []int{}
	query, state := "", ""
	for {
		page, err := client.ListPage(parent.Context(), "", 50, offset, query, state)
		if err != nil {
			return err
		}
		items := []selector.Item{}
		byID := map[string]filetransfer.Manifest{}
		for _, item := range page.Items {
			items = append(items, selector.Item{ID: item.TransferID, Title: item.Basename, Description: item.State + " · " + strconv.FormatInt(item.Size, 10) + " bytes"})
			byID[item.TransferID] = item
		}
		items = append(items, selector.Item{ID: "refresh", Title: "Refresh", Action: true}, selector.Item{ID: "filter", Title: "Search or filter", Action: true}, selector.Item{ID: "default", Title: "Use as default destination", Description: title, Action: true})
		if page.Pagination.NextOffset != nil {
			items = append(items, selector.Item{ID: "next", Title: "Next page", Action: true})
		}
		if len(history) > 0 {
			items = append(items, selector.Item{ID: "previous", Title: "Previous page", Action: true})
		}
		choice, err := chooseHomeAction(parent, "Sent files to "+title, items)
		if err != nil {
			return err
		}
		switch choice.ID {
		case "refresh":
			offset = 0
			history = nil
		case "next":
			history = append(history, offset)
			offset = *page.Pagination.NextOffset
		case "previous":
			offset = history[len(history)-1]
			history = history[:len(history)-1]
		case "default":
			if err := runHomeResult(parent, []string{"send", "destination", "set", machineID}); err != nil {
				return err
			}
		case "filter":
			nextQuery, err := prompt.Text(prompt.TextOptions{Context: parent.Context(), Title: "File name or transfer ID", Initial: query, Stdin: os.Stdin, Output: parent.ErrOrStderr()})
			if interactiveCanceled(err) {
				continue
			}
			if err != nil {
				return err
			}
			nextState, err := prompt.Text(prompt.TextOptions{Context: parent.Context(), Title: "Transfer state", Description: "Empty matches all states", Initial: state, Stdin: os.Stdin, Output: parent.ErrOrStderr()})
			if interactiveCanceled(err) {
				continue
			}
			if err != nil {
				return err
			}
			query, state = nextQuery, nextState
			offset = 0
			history = nil
		default:
			item := byID[choice.ID]
			actions := []selector.Item{{ID: "details", Title: "Details", Description: item.State}}
			if item.State == "created" || item.State == "uploading" || item.State == "pending" {
				actions = append(actions, selector.Item{ID: "cancel", Title: "Cancel delivery", Description: "Cancel the complete batch"})
			}
			action, err := chooseHomeAction(parent, item.Basename, actions)
			if interactiveCanceled(err) {
				continue
			}
			if err != nil {
				return err
			}
			if action.ID == "details" {
				err = showHomeText(parent, item.Basename, fmt.Sprintf("Transfer: %s\nState: %s\nSize: %d bytes\nCommitted: %d bytes\nResult: %s\nReceipt: %s", item.TransferID, item.State, item.Size, item.CommittedOffset, item.ResultCode, item.ReceiptPath))
			} else {
				yes, confirmErr := prompt.Confirm(prompt.ConfirmOptions{Context: parent.Context(), Title: "Cancel this transfer batch?", Description: item.Basename, Stdin: os.Stdin, Output: parent.ErrOrStderr()})
				if interactiveCanceled(confirmErr) {
					continue
				}
				if confirmErr != nil {
					return confirmErr
				}
				if !yes {
					continue
				}
				err = client.Cancel(parent.Context(), item.TransferID)
			}
			if err != nil && !interactiveCanceled(err) {
				if err := showHomeFailure(parent, err); err != nil {
					return err
				}
			}
		}
	}
}
