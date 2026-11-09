// Package causetext keeps the cause text of an unexpected failure locally. Diagnostic events carry only
// an error's type chain, so without this an intermittent internal failure cannot be traced; the record is
// owner-only and never uploaded.
package causetext

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
)

// Text lists the distinct leaf errors under err, bounded in depth
// and length. A CommandFailure's own message is public prose; the leaves are
// the diagnostic causes it retains. A single wrapper that hides its cause (a step name over the real
// error) is looked through; one whose text already ends with its only cause stands for it.
func Text(err error) string {
	leaves := leafTexts(err, 0)
	if len(leaves) > 8 {
		leaves = leaves[:8]
	}
	return strings.Join(leaves, "; ")
}

func leafTexts(current error, depth int) []string {
	if current == nil {
		return nil
	}
	var children []error
	if joined, ok := current.(interface{ Unwrap() []error }); ok {
		children = joined.Unwrap()
	} else if single, ok := current.(interface{ Unwrap() error }); ok && single.Unwrap() != nil {
		children = []error{single.Unwrap()}
	}
	if len(children) == 0 || depth >= 12 {
		text := current.Error()
		if len(text) > 300 {
			text = text[:300] + "..."
		}
		return []string{text}
	}
	var leaves []string
	for _, child := range children {
		for _, text := range leafTexts(child, depth+1) {
			if !slices.Contains(leaves, text) {
				leaves = append(leaves, text)
			}
		}
	}
	if _, joined := current.(interface{ Unwrap() []error }); !joined && len(leaves) == 1 && strings.HasSuffix(current.Error(), leaves[0]) {
		return []string{current.Error()}
	}
	return leaves
}

// Record writes the operation, support reference and Text(err) to directory/name (0600), replacing the
// previous record.
func Record(directory, name, operation, supportReference string, err error) error {
	if directory == "" || err == nil {
		return nil
	}
	if mkErr := os.MkdirAll(directory, 0o700); mkErr != nil {
		return mkErr
	}
	body := time.Now().UTC().Format(time.RFC3339) + " " + operation + " " + supportReference + "\n" + Text(err) + "\n"
	return atomicfile.Write(filepath.Join(directory, name), []byte(body), atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1})
}
