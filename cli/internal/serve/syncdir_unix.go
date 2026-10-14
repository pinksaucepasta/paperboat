//go:build unix

package serve

import "os"

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return serveOperationFailure("could not sync selected serve Inbox directory", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return serveOperationFailure("could not sync selected serve Inbox directory", syncErr, closeErr)
}
