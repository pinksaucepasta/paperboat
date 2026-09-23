package daemonrpc

import (
	"context"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"google.golang.org/grpc/metadata"
)

func TestRPCSupportReferenceRoundTripContext(t *testing.T) {
	reference := "pb-0123456789abcdef0123456789abcdef"
	outgoing := rpcContext(supportref.WithContext(context.Background(), reference))
	metadataOut, ok := metadata.FromOutgoingContext(outgoing)
	if !ok || len(metadataOut.Get(supportReferenceMetadata)) != 1 {
		t.Fatal("support metadata missing")
	}
	incoming := metadata.NewIncomingContext(context.Background(), metadata.Pairs(supportReferenceMetadata, reference))
	if got := supportref.FromContext(incomingSupportContext(incoming)); got != reference {
		t.Fatalf("reference=%q", got)
	}
}
