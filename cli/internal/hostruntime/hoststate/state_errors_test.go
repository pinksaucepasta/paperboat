package hoststate

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSnapshotParserRetainsCauseWithoutExposingRecord(t *testing.T) {
	const privateValue = "PRIVATE_SNAPSHOT_RECORD_VALUE"
	payload := []byte(`{"schema":"paperboat.preview-tunnel/v1","kind":"tunnel_config_snapshot","tunnel_id":"tun_01","generation":1,"private":"` + privateValue + `","broken":@}`)
	_, err := ParseTunnelConfigSnapshot(payload, "tun_01", 1)
	var syntaxErr *json.SyntaxError
	if err == nil || !errors.Is(err, ErrInvalidState) || !errors.As(err, &syntaxErr) || err.Error() != "tunnel configuration snapshot JSON is invalid" || strings.Contains(err.Error(), privateValue) {
		t.Fatalf("snapshot error did not safely retain parser cause: %v", err)
	}
}

func TestSnapshotCredentialRejectionDoesNotExposeUntrustedFieldName(t *testing.T) {
	const (
		privateField = "PRIVATE_FIELD_NAME_MARKER_private_key"
		privateValue = "PRIVATE_CREDENTIAL_VALUE"
	)
	payload := strings.TrimSuffix(string(snapshotFixturePayload(1)), "}") + `,"` + privateField + `":"` + privateValue + `"}`
	_, err := NewConfigSnapshot("tun_01", 1, []byte(payload))
	if err == nil || !errors.Is(err, ErrCredentialMaterial) || strings.Contains(err.Error(), privateField) || strings.Contains(err.Error(), privateValue) {
		t.Fatalf("credential rejection exposed input: %v", err)
	}
}
