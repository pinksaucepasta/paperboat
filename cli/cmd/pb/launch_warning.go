package main

import "github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"

// launchWarning is the typed form of a launch notice in JSON output.
type launchWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

const (
	environmentPendingMessage       = "Paperboat ENV not applied: delivery to this machine is pending. The command ran without your managed values; run `pb env vault unlock` on any of your devices."
	environmentUndeliverableMessage = "Paperboat ENV not applied: delivery to this machine failed. The command ran without your managed values; run `pb env host show` to see its delivery state."
)

func launchWarnings(codes []string) []launchWarning {
	var out []launchWarning
	for _, code := range codes {
		switch code {
		case envinject.LaunchNoticePending:
			out = append(out, launchWarning{Code: code, Message: environmentPendingMessage})
		case envinject.LaunchNoticeUndeliverable:
			out = append(out, launchWarning{Code: code, Message: environmentUndeliverableMessage})
		}
	}
	return out
}

// launchWarningLine is the one stderr line shown for a launch's notices.
func launchWarningLine(codes []string) string {
	for _, warning := range launchWarnings(codes) {
		return warning.Message
	}
	return ""
}
