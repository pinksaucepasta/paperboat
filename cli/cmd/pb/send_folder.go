package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/pinksaucepasta/paperboat/internal/filetransfer"
)

// folderNotice says what a folder send carries and, once, what was left out (links and special files are skipped the
// same way on every OS, as rsync skips them without -l), so nothing is dropped unseen.
func folderNotice(out io.Writer, sources []filetransfer.Source) {
	for _, source := range sources {
		if source.Folder == nil {
			continue
		}
		fmt.Fprintf(out, "Folder %s: %d files, %d folders, %s\n", source.Basename, source.Folder.Files, len(source.Folder.Paths)-source.Folder.Files, transferSize(source.Size))
		for i, skipped := range source.Folder.Skipped {
			if i == 5 {
				fmt.Fprintf(out, "  ... and %d more not sent\n", len(source.Folder.Skipped)-i)
				break
			}
			fmt.Fprintf(out, "  not sent: %s/%s (%s; Paperboat sends files and folders only)\n", source.Basename, skipped.Path, skipped.Kind)
		}
	}
}

// folderToThisComputer refuses a folder sent to the computer you are on: that send is a copy into its own Inbox, which
// takes files. Nothing was sent.
func folderToThisComputer(sources []filetransfer.Source) error {
	for _, source := range sources {
		if source.Folder != nil {
			message := fmt.Sprintf("%s is a folder; sending to this computer copies files into its Inbox and has no folder form. Copy the folder yourself, or send it to another machine.", source.Basename)
			return usageError{err: errors.New(message), publicMessage: message, refusal: true}
		}
	}
	return nil
}

// folderSuffix is the receipt line's note on a folder: how many files it holds.
func folderSuffix(source filetransfer.Source) string {
	if source.Folder == nil {
		return ""
	}
	return fmt.Sprintf(" (folder, %d files)", source.Folder.Files)
}

// folderChanged names a file that changed while its folder was being sent: the send stopped and nothing was delivered.
func folderChanged(err error) error {
	var changed *filetransfer.SourceChangedError
	if !errors.As(err, &changed) {
		return err
	}
	message := fmt.Sprintf("%s changed while the folder was being sent, so nothing was delivered. Send the folder again when nothing is writing to it.", changed.Path)
	return usageError{err: err, publicMessage: message, refusal: true}
}
