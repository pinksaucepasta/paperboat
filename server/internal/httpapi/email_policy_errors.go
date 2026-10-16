package httpapi

import (
	"errors"
	"net/http"

	"github.com/pinksaucepasta/paperboat-server/internal/emailpolicy"
)

func writeEmailPolicyError(w http.ResponseWriter, r *http.Request, err error) bool {
	status, code := 0, ""
	var policy error
	switch {
	case errors.Is(err, emailpolicy.ErrInvalid):
		status, code, policy = 400, "invalid_email", emailpolicy.ErrInvalid
	default:
		return false
	}
	logHTTPRejection(r, status, code, policy)
	writeError(w, r, status, code, policy.Error())
	return true
}
