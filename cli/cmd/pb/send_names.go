package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/transfername"
)

// receiverNameRefusal names every file the receiving machine's OS cannot store under its name, with the reason, before
// anything is sent: the receiver would otherwise rename it (Windows drops a trailing dot), fail at publication, or make the
// sender wait for an Inbox that can never be written.
func receiverNameRefusal(alias, platform string, sources []filetransfer.Source) error {
	var reasons []string
	for _, source := range sources {
		if reason := transfername.Refusal(platform, source.Basename); reason != "" {
			reasons = append(reasons, fmt.Sprintf("%q: %s", source.Basename, reason))
		}
		if source.Folder == nil {
			continue
		}
		// A folder is held to the same rules, every file and folder inside it by its path.
		for _, rel := range source.Folder.Paths {
			if reason := transfername.FolderPathRefusal(platform, rel); reason != "" {
				reasons = append(reasons, fmt.Sprintf("%q: %s", source.Basename+"/"+rel, reason))
			}
		}
		if reason := transfername.FolderCollision(platform, source.Folder.Paths); reason != "" {
			reasons = append(reasons, fmt.Sprintf("in %q: %s", source.Basename, reason))
		}
	}
	if len(reasons) == 0 {
		return nil
	}
	many := plural(len(reasons))
	if len(reasons) > 5 {
		reasons = append(reasons[:5], fmt.Sprintf("and %d more", len(reasons)-5))
	}
	message := fmt.Sprintf("Nothing was sent: %s cannot store %s. Rename the file%s and send again.", alias, strings.Join(reasons, "; "), many)
	return usageError{err: errors.New(message), publicMessage: message, refusal: true} // the arguments are right, the name is not: no command help
}

func plural(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}
