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

// Text lists the distinct leaf errors under err, each with the step names above it ("install services: start
// hostd: context deadline exceeded"), bounded in depth and length. A CommandFailure's own message is public
// prose; the leaves are the diagnostic causes it retains.
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
	text := current.Error()
	if len(children) == 0 || depth >= 12 {
		return []string{bounded(text)}
	}
	// A wrapper's own words are a step name: what precedes its cause ("read state: %w"), or its whole text
	// when that names none of its causes (a typed step error). errors.Join has no words of its own.
	step := text
	for _, child := range children {
		if child == nil {
			continue
		}
		if len(children) == 1 && strings.HasSuffix(text, child.Error()) {
			step = strings.TrimSuffix(strings.TrimSpace(strings.TrimSuffix(text, child.Error())), ":")
			break
		}
		if strings.Contains(text, child.Error()) {
			step = ""
			break
		}
	}
	var leaves []string
	for _, child := range children {
		for _, leaf := range leafTexts(child, depth+1) {
			if step != "" && !strings.HasPrefix(leaf, step) {
				leaf = bounded(step + ": " + leaf)
			}
			if !slices.Contains(leaves, leaf) {
				leaves = append(leaves, leaf)
			}
		}
	}
	return leaves
}

func bounded(text string) string {
	if len(text) > 400 {
		return text[:400] + "..."
	}
	return text
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
