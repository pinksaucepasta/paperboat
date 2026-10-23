package controlplane

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
)

var ErrUsageSignature = errors.New("usage report signature is invalid")

type signedUsageEnvelope struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

type signedUsageKey struct {
	AuthorityID string `json:"authority_id"`
	Node        string `json:"edge_node_id"`
	Epoch       string `json:"counter_epoch"`
	Environment string `json:"environment_id"`
	Route       string `json:"route_id"`
	Direction   string `json:"direction"`
	Revision    int64  `json:"route_revision"`
}

type signedUsageDocument struct {
	OperationID string         `json:"operation_id"`
	Key         signedUsageKey `json:"key"`
	Bytes       int64          `json:"bytes"`
	Start       time.Time      `json:"interval_start"`
	End         time.Time      `json:"interval_end"`
}

func (s *EdgeService) verifyUsage(ctx context.Context, request edgeUsageRequest) error {
	if len(request.Payload) == 0 || len(request.Payload) > maxEdgeDocument {
		return usageSignatureError("payload missing or too large")
	}
	var envelope signedUsageEnvelope
	if strictUsageJSON(request.Payload, &envelope) != nil || envelope.Algorithm != "EdDSA" || envelope.KeyID == "" {
		return usageSignatureError("envelope invalid")
	}
	publicKey, err := s.store.Queries().GetActiveControlUsageVerificationKey(ctx, dbsqlc.GetActiveControlUsageVerificationKeyParams{KeyID: envelope.KeyID, EdgeNodeID: request.Node, Now: s.clock()})
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return usageSignatureError("no active verification key for this key id and edge node")
	}
	return verifySignedUsageEnvelope(ed25519.PublicKey(publicKey), envelope, request)
}

func verifySignedUsageEnvelope(publicKey ed25519.PublicKey, envelope signedUsageEnvelope, request edgeUsageRequest) error {
	payload, err := base64.RawURLEncoding.DecodeString(envelope.Payload)
	if err != nil || len(payload) > maxEdgeDocument {
		return usageSignatureError("payload encoding invalid")
	}
	signature, err := base64.RawURLEncoding.DecodeString(envelope.Signature)
	if err != nil || !ed25519.Verify(publicKey, payload, signature) {
		return usageSignatureError("signature does not verify")
	}
	var document signedUsageDocument
	if err := strictUsageJSON(payload, &document); err != nil {
		return usageSignatureError("signed document invalid: " + err.Error())
	}
	if differing := signedUsageDifferences(document, request); len(differing) > 0 {
		return usageSignatureError("signed fields differ from the report: " + strings.Join(differing, ","))
	}
	return nil
}

// usageSignatureError is ErrUsageSignature naming the step that refused the report; the
// reasons carry field names only, never values.
func usageSignatureError(reason string) error {
	return fmt.Errorf("%w: %s", ErrUsageSignature, reason)
}

func signedUsageDifferences(document signedUsageDocument, request edgeUsageRequest) []string {
	var differing []string
	for name, same := range map[string]bool{
		"authority_id": document.Key.AuthorityID == request.AuthorityID, "operation_id": document.OperationID == request.OperationID,
		"edge_node_id": document.Key.Node == request.Node, "counter_epoch": document.Key.Epoch == request.Epoch,
		"environment_id": document.Key.Environment == request.Environment, "route_id": document.Key.Route == request.Route,
		"route_revision": document.Key.Revision == request.Revision, "direction": document.Key.Direction == request.Direction,
		"bytes": document.Bytes == request.Bytes, "interval_start": document.Start.Equal(request.Start), "interval_end": document.End.Equal(request.End),
	} {
		if !same {
			differing = append(differing, name)
		}
	}
	sort.Strings(differing)
	return differing
}

func strictUsageJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrUsageSignature
	}
	return nil
}
