//go:build windows

package hostservice

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/managedssh"
)

// A revocation list can outgrow the 64 KiB request cap, which is read before the caller is trusted with anything and so
// must stay small. The client therefore sends it as a transfer of bounded chunks and the service stages them; only the
// commit, after the count and digest match, hands the whole list to the reconciler in one call. An abandoned, expired
// or inconsistent transfer applies nothing. One transfer is live at a time: a new begin replaces it.
const (
	opRevokedBegin  = "begin_ssh_revoked_transfer"
	opRevokedChunk  = "append_ssh_revoked_chunk"
	opRevokedCommit = "commit_ssh_revoked_transfer"

	revokedChunkMaxIDs      = 256
	revokedChunkMaxBytes    = 32 << 10 // the IDs of one chunk; with JSON framing a request stays under the 64 KiB cap
	revokedTransferMaxIDs   = 32768
	revokedTransferMaxBytes = 4 << 20
	revokedTransferTTL      = 2 * time.Minute
	revokedTransferIDBytes  = 16
)

type revokedTransfer struct {
	id        string
	keys      []string
	principal string
	count     int
	digest    string
	ids       []string
	bytes     int
	chunks    int
	expires   time.Time
}

func revokedDigest(ids []string) string {
	sum := sha256.New()
	for _, id := range ids {
		sum.Write([]byte(id))
		sum.Write([]byte{'\n'})
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func validTransferID(id string) bool {
	if len(id) != 2*revokedTransferIDBytes {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// revokedTransferRequest serves the three transfer operations; it reports false for any other operation.
func (s *Server) revokedTransferRequest(ctx context.Context, request Request) (Response, bool) {
	switch request.Operation {
	case opRevokedBegin, opRevokedChunk, opRevokedCommit:
	default:
		return Response{}, false
	}
	if request.Mode != "" || request.Version != 0 || request.Artifact != nil || s.config.AuthorizedKeys == nil || !validTransferID(request.TransferID) {
		return s.errorResponse("invalid_request"), true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.config.Now()
	if s.revoked != nil && !now.Before(s.revoked.expires) {
		s.revoked = nil
	}
	switch request.Operation {
	case opRevokedBegin:
		if request.AuthorizedKeys == nil || len(*request.AuthorizedKeys) > maxAuthorizedKeys || len(request.SSHRevoked) != 0 || request.ChunkIndex != 0 || request.TotalCount < 1 || request.TotalCount > revokedTransferMaxIDs || len(request.TotalDigest) != 2*sha256.Size {
			return s.errorResponse("invalid_request"), true
		}
		s.revoked = &revokedTransfer{id: request.TransferID, keys: append([]string(nil), (*request.AuthorizedKeys)...), principal: request.SSHPrincipal, count: request.TotalCount, digest: request.TotalDigest, expires: now.Add(revokedTransferTTL)}
		return s.errorResponse(""), true
	case opRevokedChunk:
		transfer := s.revoked
		if transfer == nil || transfer.id != request.TransferID || request.AuthorizedKeys != nil || request.TotalCount != 0 || request.TotalDigest != "" || request.ChunkIndex != transfer.chunks {
			return s.errorResponse("invalid_request"), true
		}
		if len(request.SSHRevoked) < 1 || len(request.SSHRevoked) > revokedChunkMaxIDs || len(transfer.ids)+len(request.SSHRevoked) > transfer.count {
			s.revoked = nil
			return s.errorResponse("invalid_request"), true
		}
		for _, id := range request.SSHRevoked {
			if !wire.ValidRevokedKeyID(id) {
				s.revoked = nil
				return s.errorResponse("invalid_request"), true
			}
			transfer.bytes += len(id)
		}
		if transfer.bytes > revokedTransferMaxBytes {
			s.revoked = nil
			return s.errorResponse("invalid_request"), true
		}
		transfer.ids = append(transfer.ids, request.SSHRevoked...)
		transfer.chunks++
		return s.errorResponse(""), true
	default: // opRevokedCommit
		transfer := s.revoked
		if transfer == nil || transfer.id != request.TransferID || request.AuthorizedKeys != nil || len(request.SSHRevoked) != 0 {
			return s.errorResponse("invalid_request"), true
		}
		s.revoked = nil
		if len(transfer.ids) != transfer.count || subtle.ConstantTimeCompare([]byte(revokedDigest(transfer.ids)), []byte(transfer.digest)) != 1 {
			return s.errorResponse("invalid_request"), true
		}
		s.mu.Unlock()
		changed, err := s.config.AuthorizedKeys.ReconcileAuthorizedKeys(ctx, transfer.keys, transfer.principal, transfer.ids)
		s.mu.Lock()
		if err != nil {
			observeReconcileFailure(ctx, err)
			return s.errorResponse("ssh_authorized_keys_reconcile_failed"), true
		}
		response := s.errorResponse("")
		response.AuthorizedKeysReconciled = true
		response.AuthorizedKeysChanged = changed
		return response, true
	}
}

// reconcileRevokedInChunks sends a revocation list too large for one request as a transfer of bounded chunks that the
// service applies only when the commit arrives.
func (c *Client) reconcileRevokedInChunks(ctx context.Context, keys []string, principal string, revoked []string) (bool, error) {
	if len(revoked) < 1 || len(revoked) > revokedTransferMaxIDs {
		return false, ErrInvalidRequest
	}
	idBytes := make([]byte, revokedTransferIDBytes)
	if _, err := rand.Read(idBytes); err != nil {
		return false, err
	}
	steps, err := revokedTransferSteps(hex.EncodeToString(idBytes), keys, principal, revoked)
	if err != nil {
		return false, err
	}
	var response Response
	for _, step := range steps {
		var err error
		if response, err = c.call(ctx, step); err != nil {
			return false, err
		}
	}
	if !response.AuthorizedKeysReconciled || response.UpdateVersion != "" {
		return false, ErrInvalidRequest
	}
	return response.AuthorizedKeysChanged, nil
}

// revokedTransferSteps splits the list into begin, bounded chunks and commit.
func revokedTransferSteps(id string, keys []string, principal string, revoked []string) ([]Request, error) {
	requestKeys := append([]string{}, keys...)
	steps := []Request{{Schema: ProtocolV1, Operation: opRevokedBegin, TransferID: id, AuthorizedKeys: &requestKeys, SSHPrincipal: principal, TotalCount: len(revoked), TotalDigest: revokedDigest(revoked)}}
	for start, index := 0, 0; start < len(revoked); index++ {
		end, size := start, 0
		for end < len(revoked) && end-start < revokedChunkMaxIDs && size+len(revoked[end]) <= revokedChunkMaxBytes {
			size += len(revoked[end])
			end++
		}
		if end == start {
			return nil, ErrInvalidRequest
		}
		steps = append(steps, Request{Schema: ProtocolV1, Operation: opRevokedChunk, TransferID: id, ChunkIndex: index, SSHRevoked: revoked[start:end]})
		start = end
	}
	return append(steps, Request{Schema: ProtocolV1, Operation: opRevokedCommit, TransferID: id}), nil
}
