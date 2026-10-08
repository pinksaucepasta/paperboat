package runtime

import (
	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
	"strings"
	"testing"
)

func TestCarrierPeerStreamInfoPreservesOpaqueIdentity(t *testing.T) {
	identity := datacarrier.Identity{AccountID: "account_" + uuid.NewString(), HostID: "machine_" + uuid.NewString(), TunnelID: "rtc_tun_existing", ConnectorID: "rtc_con_existing", SessionID: "session_" + uuid.NewString(), ProcessGeneration: 2, Generation: 3}
	info := carrierPeerStreamInfo(identity)
	if info.IDs.AccountID != identity.AccountID || info.IDs.HostID != identity.HostID || info.IDs.TunnelID != identity.TunnelID || info.IDs.ConnectorID != identity.ConnectorID || info.IDs.SessionID != identity.SessionID || info.Kind != "carrier" || info.Generations.Connector != 2 || info.Generations.Session != 3 {
		t.Fatalf("carrier context lost: %+v", info)
	}
	parsed, err := uuid.Parse(strings.TrimPrefix(info.CorrelationID, "correlation_"))
	if err != nil || parsed.Version() != 4 || parsed.Variant() != uuid.RFC4122 || "correlation_"+parsed.String() != info.CorrelationID {
		t.Fatalf("carrier correlation = %q", info.CorrelationID)
	}
	if second := carrierPeerStreamInfo(identity); second.CorrelationID == info.CorrelationID {
		t.Fatal("carrier attempts reused correlation")
	}
}
