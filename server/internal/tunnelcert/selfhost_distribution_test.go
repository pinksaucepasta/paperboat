package tunnelcert

import (
	"context"
	"errors"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/nodeauth"
)

type installationCredentials struct{ unavailable bool }

func (c installationCredentials) RuntimeNode(_ context.Context, token string) (string, error) {
	if !c.unavailable && token == "private-runtime-credential-0123456789" {
		return "private_node", nil
	}
	return "", errors.New("unavailable")
}
func (c installationCredentials) RuntimeCredential(context.Context, string) (string, error) {
	if c.unavailable {
		return "", errors.New("unavailable")
	}
	return "private-runtime-credential-0123456789", nil
}
func (c installationCredentials) IsSelfHosted(_ context.Context, node string) (bool, error) {
	return node == "private_node", nil
}

func TestSelfhostDistributionUsesExactNodeCredential(t *testing.T) {
	managed := []byte("managed-runtime-credential-0123456789")
	hub := &DistributionHub{credential: managed, nodeCredentials: installationCredentials{}}
	message := DistributionMessage{EdgeNodeID: "private_node", Version: 1}
	var err error
	message.Proof, err = hub.sign(context.Background(), message)
	if err != nil {
		t.Fatal(err)
	}
	if VerifyDistributionMessageProof(message, []byte("private-runtime-credential-0123456789")) != nil {
		t.Fatal("private node cannot verify own envelope")
	}
	if VerifyDistributionMessageProof(message, managed) == nil {
		t.Fatal("private envelope uses managed credential")
	}
	hub.nodeCredentials = installationCredentials{unavailable: true}
	if _, err = hub.sign(context.Background(), message); err == nil {
		t.Fatal("unavailable private credential silently fell back")
	}
}

type hostedNodeCredentials struct{ unavailable bool }

func (c hostedNodeCredentials) RuntimeNode(context.Context, string) (string, error) {
	return "hosted_node", nil
}
func (c hostedNodeCredentials) RuntimeCredential(context.Context, string) (string, error) {
	if c.unavailable {
		return "", errors.New("revoked hosted credential")
	}
	return "hosted-scoped-credential-0123456789012345", nil
}
func (c hostedNodeCredentials) IsSelfHosted(context.Context, string) (bool, error) { return false, nil }
func TestHostedDistributionUsesScopedNodeCredential(t *testing.T) {
	fleet := []byte("fleet-global-credential-0123456789012345")
	hub := &DistributionHub{credential: fleet, nodeCredentials: hostedNodeCredentials{}}
	message := DistributionMessage{EdgeNodeID: "hosted_node", Version: 1}
	var err error
	message.Proof, err = hub.sign(context.Background(), message)
	if err != nil || VerifyDistributionMessageProof(message, []byte("hosted-scoped-credential-0123456789012345")) != nil {
		t.Fatal("hosted node cannot verify its scoped envelope", err)
	}
	if VerifyDistributionMessageProof(message, fleet) == nil {
		t.Fatal("scoped hosted envelope signed with fleet secret")
	}
	hub.nodeCredentials = hostedNodeCredentials{unavailable: true}
	if _, err = hub.sign(context.Background(), message); err == nil {
		t.Fatal("unavailable scoped credential fell back to fleet secret")
	}
}

type mixedMissingNodeCredentials struct{}

func (mixedMissingNodeCredentials) RuntimeNode(context.Context, string) (string, error) {
	return "", errors.Join(nodeauth.ErrNotFound, syscall.ECONNREFUSED)
}
func (mixedMissingNodeCredentials) RuntimeCredential(context.Context, string) (string, error) {
	return "", errors.Join(nodeauth.ErrNotFound, syscall.ECONNREFUSED)
}
func (mixedMissingNodeCredentials) IsSelfHosted(context.Context, string) (bool, error) {
	return false, nil
}

func TestDistributionSigningPreservesResolverOutageWithoutFleetFallback(t *testing.T) {
	fleet := []byte("fleet-global-credential-0123456789012345")
	hub := &DistributionHub{credential: fleet, nodeCredentials: mixedMissingNodeCredentials{}}
	_, err := hub.sign(context.Background(), DistributionMessage{EdgeNodeID: "edge_01", Version: 1})
	if !errors.Is(err, syscall.ECONNREFUSED) || !errors.Is(err, ErrDistributionTransportFailed) {
		t.Fatalf("mixed resolver outage lost its cause: %v", err)
	}
	if err.Error() != ErrDistributionTransportFailed.Error() {
		t.Fatalf("distribution failure exposed resolver detail: %q", err.Error())
	}
}
