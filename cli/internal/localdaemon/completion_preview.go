package localdaemon

import (
	"context"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

func previewCompletionItems(ctx context.Context, client *api.Client, machines []api.UserMachine, maximum int) ([]localapi.CompletionItem, error) {
	items := make([]localapi.CompletionItem, 0)
	environments := make(map[string]string, len(machines))
	for _, machine := range machines {
		environments[machine.ID] = machine.EnvironmentID
	}
	seen := make(map[string]bool)
	cursor := ""
	for len(items) < maximum {
		if seen[cursor] {
			return nil, api.ErrPreviewLeaseInvalid
		}
		seen[cursor] = true
		page, err := client.ListPreviewLeases(ctx, cursor, min(200, maximum-len(items)))
		if err != nil {
			return nil, err
		}
		for _, preview := range page.Items {
			items = append(items, localapi.CompletionItem{Kind: "preview", Value: preview.ID, Description: completionDescription(preview.Endpoint, preview.State), EnvironmentID: environments[preview.OwnerMachineID]})
		}
		if page.NextCursor == "" {
			break
		}
		if len(page.Items) == 0 {
			return nil, api.ErrPreviewLeaseInvalid
		}
		cursor = page.NextCursor
	}
	return items, nil
}
