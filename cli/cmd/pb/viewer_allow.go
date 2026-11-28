package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/shared/viewerallow"
	"github.com/spf13/cobra"
)

const viewerAllowFlag = "allow"

// tunnelViewerAllowStage names the tunnel-create step that saves the --allow list.
const tunnelViewerAllowStage = "saving its --allow list"

// How long, and how often, saving an allow list is retried while the new route is not visible yet or the server asks.
var (
	viewerAllowSaveBudget = 20 * time.Second
	viewerAllowSavePause  = 500 * time.Millisecond
)

func addViewerAllowFlag(command *cobra.Command, noun string) {
	command.Flags().StringSlice(viewerAllowFlag, nil, "let these people open the private "+noun+" too, after signing in: email addresses or *@domain (comma-separated or repeatable; implies --private; changes apply when the edge next reaches Paperboat, usually within 10 s)")
}

// viewerAllowList is the checked --allow list, or nil without the flag. It is checked before anything is created, by
// the rules the server applies.
func viewerAllowList(command *cobra.Command) ([]string, error) {
	flag := command.Flags().Lookup(viewerAllowFlag)
	if flag == nil || !flag.Changed {
		return nil, nil
	}
	values, err := command.Flags().GetStringSlice(viewerAllowFlag)
	if err != nil {
		return nil, err
	}
	for i := range values {
		values[i] = strings.TrimSpace(values[i])
	}
	list, err := viewerallow.Canonical(values)
	if err != nil {
		return nil, localArgumentError("--allow: " + strings.TrimPrefix(err.Error(), viewerallow.ErrInvalid.Error()+": "))
	}
	return list, nil
}

// saveViewerAllowList makes the route's browser audience exactly this list. The resource is already private, so until
// this succeeds nobody but its owner can open it.
func saveViewerAllowList(ctx context.Context, client *api.Client, kind, resourceID, routeID string, list []string) error {
	ctx, cancel := context.WithTimeout(ctx, viewerAllowSaveBudget)
	defer cancel()
	for {
		err := func() error {
			current, err := client.GetViewerPolicy(ctx, kind, resourceID, routeID)
			if err != nil {
				return err
			}
			if current.Configured && sameViewerAllowList(current, list) {
				return nil // saved already: a resumed create, or an answer lost on the way back
			}
			saved, err := client.SetViewerPolicy(ctx, kind, resourceID, routeID, api.ViewerPolicyInput{Audience: api.ViewerAudienceSpecificEmails, AllowedEmails: list, ExpectedGeneration: current.Generation})
			if err != nil {
				return err
			}
			if !saved.Configured || !sameViewerAllowList(saved, list) {
				return errors.New("the server did not answer with the saved allow list")
			}
			return nil
		}()
		if err == nil || !retryViewerAllowSave(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(viewerAllowSavePause):
		}
	}
}

func sameViewerAllowList(policy api.ViewerPolicy, list []string) bool {
	if policy.Audience != api.ViewerAudienceSpecificEmails || len(policy.AllowedEmails) != len(list) {
		return false
	}
	have, want := slices.Clone(policy.AllowedEmails), slices.Clone(list)
	slices.Sort(have)
	slices.Sort(want)
	return slices.Equal(have, want)
}

// retryViewerAllowSave reports an answer a moment can change: a route the server does not list yet, a policy saved
// meanwhile, or an unavailable server. Every other refusal is final.
func retryViewerAllowSave(err error) bool {
	var refusal *api.APIError
	if !errors.As(err, &refusal) {
		return false
	}
	return refusal.Code == "not_found" || refusal.Code == "viewer_policy_conflict" || refusal.Code == "viewer_policy_unavailable"
}

// savePreviewViewerAllowList saves the list on a ready preview's route.
func savePreviewViewerAllowList(ctx context.Context, client *api.Client, previewID string, list []string) error {
	lease, err := client.GetPreviewLease(ctx, previewID)
	if err != nil {
		return err
	}
	if lease.ID != previewID || lease.RouteID == "" || lease.AccessMode != "private" {
		return errors.New("the server did not return this private preview's route")
	}
	return saveViewerAllowList(ctx, client, "preview", previewID, lease.RouteID, list)
}

// viewerAllowFailureDetail is the short, safe reason a refusal message shows.
func viewerAllowFailureDetail(err error) string {
	var refusal *api.APIError
	if errors.As(err, &refusal) && safeViewerAllowCode(refusal.Code) {
		return refusal.Code
	}
	return ""
}

func safeViewerAllowCode(code string) bool {
	return code != "" && len(code) <= 64 && strings.Trim(code, "abcdefghijklmnopqrstuvwxyz0123456789_") == ""
}

func viewerAllowSentence(list []string) string {
	return "Who can open it: you and " + strings.Join(list, ", ") + ", after signing in with a verified email"
}
