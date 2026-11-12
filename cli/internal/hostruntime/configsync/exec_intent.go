package configsync

import (
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/filemode"
)

// keepExecutableIntent stages a file as executable when HEAD has it executable. A working tree without POSIX bits (Windows)
// cannot express the mode, so staging would turn every executable file into a 100644 change: a mode flip nobody made that
// conflicts with the peer and drops the executable intent when the file is edited there (X-04).
func keepExecutableIntent(repository *git.Repository) error {
	head, err := repository.Head()
	if err != nil {
		return err
	}
	commit, err := repository.CommitObject(head.Hash())
	if err != nil {
		return err
	}
	tree, err := commit.Tree()
	if err != nil {
		return err
	}
	index, err := repository.Storer.Index()
	if err != nil {
		return err
	}
	changed := false
	for _, entry := range index.Entries {
		if entry.Mode != filemode.Regular {
			continue
		}
		if file, err := tree.File(entry.Name); err == nil && file.Mode == filemode.Executable {
			entry.Mode, changed = filemode.Executable, true
		}
	}
	if !changed {
		return nil
	}
	return repository.Storer.SetIndex(index)
}

// repositoryClean reports that nothing is staged. Where files carry no POSIX bits the working-tree column of the status
// reports every executable file as modified for its mode alone; that is not a change to publish (an empty commit would be
// pushed on every cycle and fail against the peer's head), so only what is staged counts.
func repositoryClean(status git.Status) bool {
	if platformHasExecBits {
		return status.IsClean()
	}
	return stagedClean(status)
}

func stagedClean(status git.Status) bool {
	for _, file := range status {
		if file.Staging != git.Unmodified {
			return false
		}
	}
	return true
}
