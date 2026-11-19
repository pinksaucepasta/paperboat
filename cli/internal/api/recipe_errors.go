package api

import (
	"fmt"
	"strings"
	"time"
)

// recipeMessages are the recipe refusals (shared/recipe/README.md "Errors"); recipeErrorMessage
// fills in the names and dates the server sends in details.
var recipeMessages = map[string]string{
	"invalid_recipe":        "The recipe file is not valid. Fix the listed errors and try again.",
	"invalid_recipe_reach":  "The recipe's [[reach]] names something this workspace does not have. Fix it and try again.",
	"unknown_vault":         "The recipe reads an ENV vault this workspace does not have. List them with `pb env vault list`.",
	"unknown_repository":    "The recipe names a config repository that is not connected. Connect one with `pb config repository connect <url>`.",
	"vault_key_missing":     "The recipe reads an ENV vault whose key could not be wrapped for it. Unlock ENV with `pb env unlock`, then run the command again.",
	"recipe_name_taken":     "A recipe with this name already exists. Change it with `pb recipe update -f <file>`.",
	"recipe_token_taken":    "This recipe token id is already used. Run the command again; it makes a new token.",
	"confirmation_required": "Run the command again with the confirmation code it printed.",
	"recipe_invalid":        "This recipe token is not valid: the recipe was revoked or the token was mistyped.",
	"recipe_expired":        "This recipe token expired. Get a new token with `pb recipe rotate <recipe>` (its machines keep working), then update PB_RECIPE.",
	"recipe_replaced":       "This recipe token was replaced (`pb recipe rotate`). Use the new token; update PB_RECIPE where this one is stored.",
	"recipe_full":           "This recipe has reached its max_machines. A machine leaves the recipe when it is removed; to allow more, raise max_machines and run `pb recipe update`.",
	"vault_in_use":          "A recipe reads this vault. Remove it from the recipe (`pb recipe update`) first.",
	"vault_name_taken":      "A vault with this name already exists in this workspace.",
}

func init() {
	for code, message := range recipeMessages {
		publicAPIMessages[code] = message
	}
}

// recipeErrorMessage is the recipe refusal with the names and dates from details.
func recipeErrorMessage(code string, details map[string]any) (string, bool) {
	text := func(key string) string { v, _ := details[key].(string); return v }
	switch code {
	case "invalid_recipe":
		var lines []string
		list, _ := details["errors"].([]any)
		for _, item := range list {
			if e, ok := item.(map[string]any); ok {
				if line, _ := e["text"].(string); line != "" {
					lines = append(lines, line)
				}
			}
		}
		if len(lines) > 0 {
			return strings.Join(lines, "\n"), true
		}
	case "unknown_vault", "unknown_repository":
		name, suggestion := text("name"), text("suggestion")
		if name == "" {
			break
		}
		var known []string
		list, _ := details["known"].([]any)
		for _, k := range list {
			if s, ok := k.(string); ok {
				known = append(known, s)
			}
		}
		message := fmt.Sprintf("vault %q does not exist in this workspace.", name)
		if code == "unknown_repository" {
			message = fmt.Sprintf("repository %q is not connected in this workspace.", name)
		}
		if len(known) > 0 {
			message += " Known: " + strings.Join(known, ", ") + "."
		}
		if suggestion != "" {
			message += fmt.Sprintf(" Did you mean %q?", suggestion)
		}
		if code == "unknown_repository" {
			message += " Connect one with `pb config repository connect <url>`."
		}
		return message, true
	case "vault_key_missing":
		if vault := text("vault"); vault != "" {
			return fmt.Sprintf("The recipe reads vault %s, and its key could not be wrapped for the recipe on this computer. Unlock ENV with `pb env unlock`, then run the command again.", vault), true
		}
	case "recipe_expired", "recipe_replaced":
		recipe := text("recipe")
		at, err := time.Parse(time.RFC3339Nano, text("at"))
		if recipe == "" || err != nil {
			break
		}
		when := at.Local().Format("2006-01-02 15:04 MST")
		if code == "recipe_expired" {
			return fmt.Sprintf("This recipe token expired on %s. Get a new token with `pb recipe rotate %s` (its machines keep working), then update PB_RECIPE.", when, recipe), true
		}
		return fmt.Sprintf("This recipe token was replaced on %s (`pb recipe rotate %s`). Use the new token; update PB_RECIPE where this one is stored.", when, recipe), true
	case "recipe_full":
		recipe := text("recipe")
		max, _ := details["max_machines"].(float64)
		if recipe != "" && max >= 1 {
			return fmt.Sprintf("Recipe %s already has %d of %d machines. A machine leaves the recipe when it is removed (`pb machine revoke <machine>`, or by remove_after_offline). To allow more, raise max_machines and run `pb recipe update -f %s.jsonc`.", recipe, int64(max), int64(max), recipe), true
		}
	}
	return "", false
}
