package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// List commands show one bounded page with the server's total instead of loading everything (PQ-04).
const (
	defaultListLimit = 50
	maxListLimit     = 200
	// maxListAll bounds --all: an account's largest list (10,000 machines) in at most 50 server pages.
	maxListAll = 10000
)

func addListPaging(command *cobra.Command) {
	command.Flags().Int("limit", defaultListLimit, fmt.Sprintf("show at most this many items (1-%d)", maxListLimit))
	command.Flags().Bool("all", false, fmt.Sprintf("show every item, up to %d", maxListAll))
}

// listPagingMax is how many items the command reads: --limit, or the --all bound.
func listPagingMax(command *cobra.Command) (int, error) {
	if all, _ := command.Flags().GetBool("all"); all {
		return maxListAll, nil
	}
	limit, _ := command.Flags().GetInt("limit")
	if limit < 1 || limit > maxListLimit {
		return 0, usageError{publicMessage: fmt.Sprintf("--limit must be between 1 and %d; use --all for more", maxListLimit)}
	}
	return limit, nil
}

// writeListFooter says when the list shown is not the whole list.
func writeListFooter(writer io.Writer, shown, total int, noun string) {
	if total > shown {
		fmt.Fprintf(writer, "Showing %d of %d %s. Use --limit or --all for more, or narrow the list with --q.\n", shown, total, noun)
	}
}
