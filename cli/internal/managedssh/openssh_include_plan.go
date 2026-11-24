//go:build darwin || linux || windows

package managedssh

import "bytes"

const openSSHIncludeLine = "Include ~/.ssh/paperboat_config # " + openSSHIncludeMarker + "\n"

// planOpenSSHInclude returns the user's config with exactly one Include line, before the first Host or Match block:
// as it is when the recorded line is still there, else with the line put (back) in. record may be nil.
func planOpenSSHInclude(main []byte, record *openSSHInstallRecord) (next []byte, chunk string, err error) {
	if openSSHIncludeIntact(main, record) {
		return main, record.IncludeChunk, nil
	}
	main = bytes.ReplaceAll(main, []byte(openSSHIncludeLine), nil)
	// What is left is not a line pb wrote, so it is not pb's to remove.
	if bytes.Contains(main, []byte(openSSHIncludeMarker)) || bytes.Contains(main, []byte(openSSHBeginMarker)) {
		return nil, "", ErrOpenSSHConfigConflict
	}
	offset := firstOpenSSHBlockOffset(main)
	chunk = openSSHIncludeLine
	if offset > 0 && main[offset-1] != '\n' {
		chunk = "\n" + chunk
	}
	next = append(append(append([]byte(nil), main[:offset]...), chunk...), main[offset:]...)
	return next, chunk, nil
}

// openSSHIncludeIntact reports whether the recorded Include line is there once and applies to every host: inside a
// Host or Match block it would apply only there.
func openSSHIncludeIntact(main []byte, record *openSSHInstallRecord) bool {
	if record == nil || !validRecordedInclude(record.IncludeChunk, openSSHIncludeLine) || bytes.Count(main, []byte(openSSHIncludeMarker)) != 1 {
		return false
	}
	at := bytes.Index(main, []byte(record.IncludeChunk))
	return at >= 0 && at <= firstOpenSSHBlockOffset(main)
}
