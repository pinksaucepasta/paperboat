package environment

import (
	"encoding/hex"
	"testing"
)

func TestOperationIdentityPreservesSignedBytes(t *testing.T) {
	raw := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	id := operationID(raw)
	if id != "envop_"+hex.EncodeToString(raw) {
		t.Fatalf("signed operation identity changed: %s", id)
	}
	for _, id := range []string{"operation_123e4567-e89b-42d3-a456-426614174000", "envop_0123456789abcdef0123456789abcdef"} {
		if !operationExpression.MatchString(id) {
			t.Fatalf("valid stored/current ID rejected %s", id)
		}
	}
	if operationExpression.MatchString("operation_/unsafe") {
		t.Fatal("unsafe identifier accepted")
	}
}
