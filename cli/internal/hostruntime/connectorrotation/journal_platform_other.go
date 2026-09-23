//go:build !darwin && !linux && !windows

package connectorrotation

import "errors"

var errJournalFileNotExist = errors.New("connector rotation journal file does not exist")

func ensurePrivateJournalDirectory(string) error           { return ErrInvalidConfig }
func readPrivateJournalFile(string, int64) ([]byte, error) { return nil, ErrJournalCorrupt }
func writePrivateJournalFile(string, []byte) error         { return ErrJournalUncertain }
