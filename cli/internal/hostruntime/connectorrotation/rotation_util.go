package connectorrotation

import (
	"encoding/base64"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
)

func typed(code connectorprotocol.Code, reason connectorprotocol.DisconnectReason, retryable bool, cause error) error {
	return &connectorprotocol.Error{Code: code, Reason: reason, Retryable: retryable, Cause: cause}
}

func nowUTC(clock Clock) time.Time {
	if clock == nil {
		return time.Now().UTC()
	}
	return clock.Now().UTC()
}

func installAck(install connectorprotocol.CredentialRotationInstall) connectorprotocol.CredentialRotationAck {
	return connectorprotocol.CredentialRotationAck{AccountID: install.AccountID, TunnelID: install.TunnelID, OperationID: install.OperationID, ConnectorID: install.ConnectorID, HostID: install.HostID, SessionID: install.SessionID, ProcessGeneration: install.ProcessGeneration, TargetSetHash: install.TargetSetHash, OldCredentialGeneration: install.OldCredentialGeneration, NewCredentialGeneration: install.NewCredentialGeneration, Status: connectorprotocol.RotationAckInstalled}
}

func revokeAck(revoke connectorprotocol.CredentialRotationRevoke) connectorprotocol.CredentialRotationAck {
	return connectorprotocol.CredentialRotationAck{AccountID: revoke.AccountID, TunnelID: revoke.TunnelID, OperationID: revoke.OperationID, ConnectorID: revoke.ConnectorID, HostID: revoke.HostID, SessionID: revoke.SessionID, ProcessGeneration: revoke.ProcessGeneration, TargetSetHash: revoke.TargetSetHash, OldCredentialGeneration: revoke.OldCredentialGeneration, NewCredentialGeneration: revoke.NewCredentialGeneration, Status: connectorprotocol.RotationAckRevoked}
}

func cloneChallenge(value connectorprotocol.CredentialRotationChallenge) *connectorprotocol.CredentialRotationChallenge {
	return &value
}

func cloneInstall(value connectorprotocol.CredentialRotationInstall) *connectorprotocol.CredentialRotationInstall {
	return &value
}

func cloneReady(value connectorprotocol.CredentialRotationReady) *connectorprotocol.CredentialRotationReady {
	return &value
}

func cloneRevoke(value connectorprotocol.CredentialRotationRevoke) *connectorprotocol.CredentialRotationRevoke {
	return &value
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalStringBytes(encoded string, raw []byte) bool {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	return err == nil && equalBytes(decoded, raw)
}

func clear(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

func validSHA256Hash(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}
