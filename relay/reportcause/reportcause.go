// Package reportcause classifies the relay's own typed errors for shared/reporting.
package reportcause

import (
	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"github.com/pinksaucepasta/paperboat/relay/nodelifecycle"
	"github.com/pinksaucepasta/paperboat/shared/reporting"
)

func init() { reporting.RegisterCause(classify) }

func classify(err error) (string, int) {
	switch {
	case reporting.SameError(err, nodelifecycle.ErrFenced):
		return "authority_rejected", 85
	case reporting.SameError(err, nodelifecycle.ErrControl):
		return "authority_unavailable", 40
	case reporting.SameError(err, derpquic.ErrAdmission):
		return "authorization_rejected", 83
	case reporting.SameError(err, derpquic.ErrProtocol):
		return "protocol_rejected", 83
	case reporting.SameError(err, derpquic.ErrOverload):
		return "capacity_exhausted", 83
	case reporting.SameError(err, derpquic.ErrExpired):
		return "authority_expired", 83
	case reporting.SameError(err, derpquic.ErrClosed):
		return "relay_closed", 82
	}
	if typed, ok := err.(*derpquic.Error); ok {
		switch typed.Code {
		case 1:
			return "authorization_rejected", 83
		case 2:
			return "protocol_rejected", 83
		case 3:
			return "capacity_exhausted", 83
		case 4:
			return "authority_expired", 83
		}
	}
	return "", 0
}
