//go:build darwin || linux

package hostruntimecmd

type purgeStageFailure struct {
	stage string
	cause error
}

func (e purgeStageFailure) Error() string           { return "Paperboat purge stage failed: " + e.stage }
func (e purgeStageFailure) Unwrap() error           { return e.cause }
func (e purgeStageFailure) DiagnosticStage() string { return e.stage }
func (e purgeStageFailure) DiagnosticCode() string  { return "purge_" + e.stage + "_failed" }
func withPurgeStage(stage string, err error) error {
	if err == nil {
		return nil
	}
	return purgeStageFailure{stage: stage, cause: err}
}
