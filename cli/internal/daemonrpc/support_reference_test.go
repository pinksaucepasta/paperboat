package daemonrpc

import (
	"context"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/supportref"
	"google.golang.org/grpc/metadata"
)

func TestRPCSupportReferenceRoundTripContext(t *testing.T) {
	reference := "support_01234567-89ab-4def-8123-456789abcdef"
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

func TestIncomingSupportContextCreatesReferenceWhenMetadataIsMissingOrInvalid(t *testing.T) {
	for name, incoming := range map[string]context.Context{
		"missing": context.Background(),
		"invalid": metadata.NewIncomingContext(context.Background(), metadata.Pairs(supportReferenceMetadata, "caller-controlled secret")),
	} {
		t.Run(name, func(t *testing.T) {
			reference := supportref.FromContext(incomingSupportContext(incoming))
			if !supportref.Valid(reference) || reference == "caller-controlled secret" {
				t.Fatalf("reference=%q is not a fresh validated support reference", reference)
			}
		})
	}
}
