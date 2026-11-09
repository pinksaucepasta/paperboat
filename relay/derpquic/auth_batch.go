package derpquic

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"tailscale.com/types/key"
)

// Commit the complete signed credential once. Publishing only its first part
// narrows the root fence and cannot authenticate a cached multipart grant on a
// new transport generation. Every part retains its signature and TLS binding.
func writeAuthRequest(w io.Writer, parts []string, namespace [32]byte, disco key.DiscoPublic) error {
	if len(parts) == 0 || len(parts) > MaxGrantParts {
		return ErrAdmission
	}
	kind, body := authFrame([]byte(parts[0]), namespace, disco)
	if namespace != ([32]byte{}) {
		binary.BigEndian.PutUint32(body[64:68], uint32(len(parts)))
	} else if len(parts) > 1 {
		kind = frameAuthBatch
		body = make([]byte, 4, 4+len(parts[0]))
		binary.BigEndian.PutUint32(body, uint32(len(parts)))
		body = append(body, parts[0]...)
	}
	if err := writeFrame(w, kind, body); err != nil {
		return err
	}
	for _, part := range parts[1:] {
		if err := writeFrame(w, frameAuth, []byte(part)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) admitAuthParts(p *peerConn, body []byte, count uint32, now time.Time) error {
	p.stream.SetReadDeadline(now.Add(5 * time.Second))
	defer p.stream.SetReadDeadline(time.Time{})
	var merged Grant
	digest := sha256.New()
	expired := false
	// Parts arrive over up to the read deadline: each is verified on the clock it arrives at, and
	// every part must still be valid when the merged grant is admitted.
	earliest := int64(math.MaxInt64)
	for i := uint32(0); i < count; i++ {
		if i != 0 {
			kind, next, err := readFrame(p.stream)
			if err != nil {
				return err
			}
			if kind != frameAuth {
				return ErrProtocol
			}
			body = next
			s.mu.Lock()
			account := s.accounts[merged.AccountID]
			s.mu.Unlock()
			if !p.rate.allow(time.Now(), len(body)) || account != nil && !account.allow(time.Now(), len(body)) {
				return ErrOverload
			}
		}
		grant, err := s.verifier.Verify(string(body), p.state(), time.Now())
		if errors.Is(err, ErrExpired) {
			expired = true
			continue
		}
		if err != nil {
			return fmt.Errorf("verify grant: %w", err)
		}
		if expired {
			continue
		}
		earliest = min(earliest, grant.ExpiresAt)
		digest.Write(grant.signedHash[:])
		if i == 0 {
			merged = grant
			continue
		}
		if !sameGrantIdentity(merged, grant) {
			return fmt.Errorf("grant part identity: %w", ErrAdmission)
		}
		merged, err = mergeGrants(merged, grant)
		if err != nil {
			return err
		}
	}
	now = time.Now()
	if expired || earliest <= now.Unix() {
		return ErrExpired
	}
	if count > 1 {
		copy(merged.signedHash[:], digest.Sum(nil))
	}
	if err := s.admit(p, merged, now); err != nil {
		return fmt.Errorf("admit: %w", err)
	}
	return nil
}
