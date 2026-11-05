package diagnostics

import (
	"bytes"
	"encoding/json"
	"slices"

	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

// MaximumBundleFailureReferences bounds how many failure references one bundle is indexed under on the server.
const MaximumBundleFailureReferences = 64

// failureReferences lists the support references of the failure records a bundle carries: error events with a
// valid support_reference, from the persisted tail and the memory ring, distinct and newest last. They come from
// the records' own structured field, never from free text; the server indexes the upload under each of them so a
// reference printed for a client-only failure finds the bundle (`pb bugreport --upload`).
func failureReferences(persisted []byte, recent []Event) []string {
	var references []string
	add := func(severity, reference string) {
		if severity != "error" || !supportref.Valid(reference) {
			return
		}
		if index := slices.Index(references, reference); index >= 0 {
			references = slices.Delete(references, index, index+1)
		}
		references = append(references, reference)
	}
	for _, line := range bytes.Split(persisted, []byte{'\n'}) {
		var record struct {
			Severity         string `json:"severity"`
			SupportReference string `json:"support_reference"`
		}
		if json.Unmarshal(line, &record) == nil {
			add(record.Severity, record.SupportReference)
		}
	}
	for _, event := range recent {
		add(event.Severity, event.SupportReference)
	}
	if len(references) > MaximumBundleFailureReferences {
		references = references[len(references)-MaximumBundleFailureReferences:]
	}
	return references
}
