package daemonrpc

import (
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Refusal is a backend answer the caller can act on. It reaches the CLI as a
// stable code, where other backend errors are masked behind a support reference.
type Refusal interface {
	error
	RefusalCode() string
}

const refusalPrefix = "refused: "

func refusalStatus(refusal Refusal) error {
	code := codes.FailedPrecondition
	if strings.HasSuffix(refusal.RefusalCode(), "not_found") {
		code = codes.NotFound
	}
	return status.Error(code, refusalPrefix+refusal.RefusalCode())
}

// RefusalCode returns the code of a refusal received from the daemon.
func RefusalCode(err error) (string, bool) {
	var remote interface{ GRPCStatus() *status.Status }
	if err == nil || !errors.As(err, &remote) {
		return "", false
	}
	message := remote.GRPCStatus().Message()
	if !strings.HasPrefix(message, refusalPrefix) {
		return "", false
	}
	return strings.TrimPrefix(message, refusalPrefix), true
}
