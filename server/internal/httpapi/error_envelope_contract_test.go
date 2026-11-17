package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/xeipuuv/gojsonschema"
)

// The schema once said requestId while every wire said request_id, and nothing read it. Real answers from the
// router (with and without details, 4xx and 5xx) must validate against it, so the two cannot drift again.
func TestErrorAnswersMatchTheErrorEnvelopeContract(t *testing.T) {
	path, err := filepath.Abs("../../../shared/testdata/contracts/schemas/common/error-envelope.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	schema, err := gojsonschema.NewSchema(gojsonschema.NewReferenceLoader("file://" + path))
	if err != nil {
		t.Fatal(err)
	}
	for name, answer := range map[string]func(http.ResponseWriter, *http.Request){
		"server failure": func(w http.ResponseWriter, r *http.Request) {
			writeError(w, r, http.StatusInternalServerError, "synthetic_failure", "The operation could not finish.")
		},
		"refusal with details": func(w http.ResponseWriter, r *http.Request) {
			writeErrorDetails(w, r, http.StatusConflict, "machine_not_ready", "Machine is not ready.", map[string]any{"machine_id": "machine_x"})
		},
	} {
		handler := NewRouter(Options{Config: config.Default(), OverrideHandler: http.HandlerFunc(answer)})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/test", nil))
		result, err := schema.Validate(gojsonschema.NewBytesLoader(response.Body.Bytes()))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !result.Valid() {
			t.Fatalf("%s: %s does not match error-envelope.schema.json: %v", name, response.Body.String(), result.Errors())
		}
		if reference := response.Header().Get("Support-Reference"); reference == "" || !strings.Contains(response.Body.String(), reference) {
			t.Fatalf("%s: the answer must carry the Support-Reference %q in its body: %s", name, reference, response.Body.String())
		}
	}
}
