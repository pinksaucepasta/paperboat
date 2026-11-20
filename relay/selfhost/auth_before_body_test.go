package selfhost

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// endlessBody is a never-ending JSON string that counts the bytes the handler pulls from it.
type endlessBody struct {
	sent  int
	limit int
}

func (b *endlessBody) Read(p []byte) (int, error) {
	if b.sent >= b.limit {
		return 0, http.ErrBodyReadAfterClose
	}
	for i := range p {
		p[i] = 'a'
	}
	if b.sent == 0 {
		copy(p, `{"claim_id":"`)
	}
	n := min(len(p), b.limit-b.sent)
	b.sent += n
	return n, nil
}
func (b *endlessBody) Close() error { return nil }

// The bearer must be verified before any request body is read.
func TestUnauthenticatedBodyIsNotReadBeforeAuth(t *testing.T) {
	dir, _, _, _ := testInstallation(t, false)
	for _, path := range []string{"/v1/selfhost/claim", "/v1/selfhost/inspect"} {
		body := &endlessBody{limit: 64 << 20}
		r := httptest.NewRequest(http.MethodPost, path, body)
		r.Header.Set("Authorization", "Bearer "+encoding.EncodeToString(make([]byte, 32)))
		w := httptest.NewRecorder()
		Handler(dir, nil, nil).ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status %d, want 401", path, w.Code)
		}
		if body.sent > 4<<10 {
			t.Fatalf("%s: read %d body bytes before authenticating", path, body.sent)
		}
	}
}
