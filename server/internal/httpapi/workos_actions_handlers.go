package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const workOSActionTolerance = 5 * time.Minute

// RegistrationAliasChecker reports the canonical address of an existing account when the submitted address is
// only another spelling of it.
type RegistrationAliasChecker interface {
	ExistingAccountForAlias(ctx context.Context, submitted string) (canonical string, exists bool, err error)
}

// workOSRegistrationAction answers WorkOS AuthKit's User Registration Action. It denies a sign-up whose address
// is an alias of an existing account's address and allows everything else; an error or an unsigned request never
// denies (the response is simply not a valid signed verdict, which the dashboard's error behavior then handles).
func workOSRegistrationAction(checker RegistrationAliasChecker, secret string, now func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if secret == "" || checker == nil {
			writeError(w, r, 503, "registration_action_unavailable", "Registration check is not configured.")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			writeError(w, r, 400, "invalid_request", "Action body is invalid or too large.")
			return
		}
		if !verifyWorkOSActionSignature(body, r.Header.Get("WorkOS-Signature"), secret, now()) {
			writeError(w, r, 401, "invalid_signature", "Action signature is invalid.")
			return
		}
		var action struct {
			Object   string `json:"object"`
			UserData struct {
				Email string `json:"email"`
			} `json:"user_data"`
		}
		if json.Unmarshal(body, &action) != nil || action.Object != "user_registration_action_context" || action.UserData.Email == "" {
			writeError(w, r, 400, "invalid_request", "Action is not a user registration.")
			return
		}
		verdict, message := "Allow", ""
		canonical, exists, err := checker.ExistingAccountForAlias(r.Context(), action.UserData.Email)
		if err != nil {
			writeError(w, r, 503, "registration_action_unavailable", "Registration check is unavailable.", err)
			return
		}
		if exists {
			verdict, message = "Deny", "An account for "+canonical+" already exists. Sign in with that address."
		}
		signed, err := signWorkOSActionResponse(verdict, message, secret, now())
		if err != nil {
			writeError(w, r, 500, "internal_error", "Could not sign the action response.", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(signed)
	}
}

// verifyWorkOSActionSignature checks `t=<unix ms>,v1=<hex HMAC-SHA256 of "t.body">` and its freshness.
func verifyWorkOSActionSignature(body []byte, header, secret string, now time.Time) bool {
	var stamp, signature string
	for _, part := range strings.Split(header, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch key {
		case "t":
			stamp = value
		case "v1":
			signature = value
		}
	}
	millis, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || signature == "" {
		return false
	}
	age := now.Sub(time.UnixMilli(millis))
	if age < -workOSActionTolerance || age > workOSActionTolerance {
		return false
	}
	got, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	return hmac.Equal(got, workOSActionMAC(secret, stamp, body))
}

func workOSActionMAC(secret, stamp string, payload []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(stamp + "."))
	mac.Write(payload)
	return mac.Sum(nil)
}

// signWorkOSActionResponse builds {"object","payload":{timestamp,verdict,error_message},"signature"}; the signature
// covers "<timestamp>.<payload JSON exactly as sent>".
func signWorkOSActionResponse(verdict, message, secret string, now time.Time) ([]byte, error) {
	if verdict != "Allow" && verdict != "Deny" || len(message) > 500 || (verdict == "Allow" && message != "") {
		return nil, errors.New("invalid action verdict")
	}
	type payload struct {
		Timestamp    int64  `json:"timestamp"`
		Verdict      string `json:"verdict"`
		ErrorMessage string `json:"error_message,omitempty"`
	}
	var inner bytes.Buffer
	encoder := json.NewEncoder(&inner)
	encoder.SetEscapeHTML(false)
	stamp := now.UnixMilli()
	if err := encoder.Encode(payload{Timestamp: stamp, Verdict: verdict, ErrorMessage: message}); err != nil {
		return nil, err
	}
	raw := bytes.TrimSpace(inner.Bytes())
	return json.Marshal(struct {
		Object    string          `json:"object"`
		Payload   json.RawMessage `json:"payload"`
		Signature string          `json:"signature"`
	}{"user_registration_action_response", raw, hex.EncodeToString(workOSActionMAC(secret, strconv.FormatInt(stamp, 10), raw))})
}
