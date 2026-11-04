package hostruntimecmd

import "strings"

// PurgeIncomplete is what the privileged cleanup reported it could not finish:
// the typed stages that failed and every Paperboat item still present.
type PurgeIncomplete struct {
	Stages    []string
	Remaining []string
}

func (e PurgeIncomplete) Error() string {
	return "Paperboat cleanup incomplete at " + strings.Join(e.Stages, ", ")
}
