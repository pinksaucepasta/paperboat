package main

import (
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
	}
	if len(reasons) == 0 {
		return nil
	}
	return localArgumentError(fmt.Sprintf("Nothing was sent: %s cannot store %s. Rename the file%s and send again.", alias, strings.Join(reasons, "; "), plural(len(reasons))))
}

func plural(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}
