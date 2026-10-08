package clientauthority

type resolutionFailure struct{ err error }

func (*resolutionFailure) Error() string { return "peer authority resolution failed" }
func (e *resolutionFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}
func (*resolutionFailure) DiagnosticStage() string { return "peer_authority" }
func (*resolutionFailure) DiagnosticCode() string  { return "peer_authority_failed" }
