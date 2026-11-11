package tunnel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

// A redial after a clean close attaches with the host's resume secret, no create and no credential, from the delivered
// cursor, and keeps the rotated secret the host answers with; the first attach's secret is kept the same way.
func TestResumeAttachesWithTheHeldSecretAndKeepsTheRotatedOne(t *testing.T) {
	first, second := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	var mu sync.Mutex
	var actions []string
	var resumed struct {
		Secret       []byte `json:"resume_secret"`
		AttachmentID string `json:"attachment_id"`
		FromSequence uint64 `json:"from_sequence"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{helperWebSocketSubprotocol}, CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			t.Error(err)
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "")
		for {
			_, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			frame, err := decodeHelperFrame(data)
			if err != nil {
				t.Error(err)
				return
			}
			if frame.Type == "hello" {
				writeHelperTestFrame(t, ws, helperFrame{Type: "welcome", RequestID: frame.RequestID, Version: helperProtocolVersion, Payload: json.RawMessage(`{"version":"1.0","capabilities":["health.v1","terminal.v1"]}`)})
				continue
			}
			if frame.Type != "request" {
				continue
			}
			var payload struct {
				Action string `json:"action"`
				Secret []byte `json:"resume_secret"`
			}
			_ = json.Unmarshal(frame.Payload, &payload)
			mu.Lock()
			actions = append(actions, payload.Action)
			mu.Unlock()
			switch payload.Action {
			case "create":
				writeHelperTestFrame(t, ws, helperFrame{Type: "response", RequestID: frame.RequestID, Version: helperProtocolVersion, Payload: json.RawMessage(`{"result":{"state":"running","generation":1,"earliest_sequence":1,"latest_sequence":9,"existing":true},"replay":false}`)})
			case "attach":
				answer := first // an attach with no secret is the first one; a resume gets the rotated secret
				if len(payload.Secret) > 0 {
					_ = json.Unmarshal(frame.Payload, &resumed)
					answer = second
				}
				body := `{"result":{"stream_id":10,"attachment_lease":1,"attachment_id":"att_held","resume_secret":"` + base64.StdEncoding.EncodeToString(answer) + `","session":{"snapshot":{"generation":1}}},"replay":true}`
				writeHelperTestFrame(t, ws, helperFrame{Type: "response", RequestID: frame.RequestID, Version: helperProtocolVersion, Payload: json.RawMessage(body)})
			}
		}
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	u.Scheme = strings.Replace(u.Scheme, "http", "ws", 1)
	u.Path = "/v1/runtime"

	attached := &resolver.TerminalTarget{Protocol: "paperboat.terminal.v1", WSSEndpoint: u.String(), Auth: resolver.AuthTarget{Method: "bearer", Token: "operation-credential"}, SessionID: "ses_held"}
	conn, err := dialTestHelperTerminal(context.Background(), attached)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if !bytes.Equal(attached.ResumeSecret, first) || attached.InputAttachmentID != "att_held" {
		t.Fatalf("the first attach kept secret=%x attachment=%q", attached.ResumeSecret, attached.InputAttachmentID)
	}

	redial := &resolver.TerminalTarget{Protocol: "paperboat.terminal.v1", WSSEndpoint: u.String(), Auth: resolver.AuthTarget{Method: "bearer"}, SessionID: "ses_held", Resume: true, ResumeSecret: attached.ResumeSecret, InputAttachmentID: attached.InputAttachmentID, AfterSequence: 7}
	conn, err = dialTestHelperTerminal(context.Background(), redial)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(actions, ","); got != "create,attach,attach" {
		t.Fatalf("host requests = %s: a resume must not create the session again", got)
	}
	if !bytes.Equal(resumed.Secret, first) || resumed.AttachmentID != "att_held" || resumed.FromSequence != 7 {
		t.Fatalf("resume attach carried secret=%x attachment=%q from=%d", resumed.Secret, resumed.AttachmentID, resumed.FromSequence)
	}
	if !bytes.Equal(redial.ResumeSecret, second) {
		t.Fatalf("the rotated secret was not kept: %x", redial.ResumeSecret)
	}
}
