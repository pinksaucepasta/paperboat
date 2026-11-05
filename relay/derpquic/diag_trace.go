package derpquic

// DiagTrace (throwaway diagnostics branch only) receives timestamped relay connection phases.
var DiagTrace func(event, detail string)

func Diag(event, detail string) {
	if f := DiagTrace; f != nil {
		f(event, detail)
	}
}
