package tunnel

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestComparisonSideIntegrityIncludesSymlinkKind(t *testing.T) {
	for _, kind := range []string{"file", "symlink"} {
		content := []byte("relative-target")
		hashed := content
		if kind == "symlink" {
			hashed = append([]byte("symlink:"), content...)
		}
		digest := sha256.Sum256(hashed)
		side := ComparisonSideMetadata{Present: true, Kind: kind, Bytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:])}
		if !VerifyComparisonSide(side, content) {
			t.Fatal("valid content rejected")
		}
		changed := append([]byte(nil), content...)
		changed[0] ^= 1
		if VerifyComparisonSide(side, changed) {
			t.Fatal("corruption accepted")
		}
	}
	if VerifyComparisonSide(ComparisonSideMetadata{Bytes: 1}, nil) {
		t.Fatal("missing content accepted")
	}
}
