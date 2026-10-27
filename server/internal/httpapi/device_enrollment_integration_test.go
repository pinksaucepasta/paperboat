package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
)

type deviceEnrollmentFixture struct {
	t       *testing.T
	router  http.Handler
	cookies []*http.Cookie
}

func (f deviceEnrollmentFixture) authorize(body map[string]any, want int) deviceGrant {
	f.t.Helper()
	body["client_id"], body["client_label"], body["device_type"], body["os"], body["scopes"] = "paperboat", "Test CLI", "desktop", "linux", cliScopes
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/device/authorize", bytes.NewReader(raw))
	req.RemoteAddr = "198.51.100.20:1234"
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != want {
		f.t.Fatalf("authorize status=%d want=%d body=%s", rec.Code, want, rec.Body.String())
	}
	var envelope struct {
		Data deviceGrant `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &envelope)
	return envelope.Data
}

func (f deviceEnrollmentFixture) request(code string) map[string]any {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/auth/device/requests/"+code, nil)
	addCookies(req, f.cookies)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("request status=%d body=%s", rec.Code, rec.Body.String())
	}
	return dataOf(f.t, rec.Body.Bytes())
}

func (f deviceEnrollmentFixture) approve(code, body string, want int) map[string]any {
	f.t.Helper()
	var reader *strings.Reader
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/device/requests/"+code+"/approve", nil)
	if body != "" {
		reader = strings.NewReader(body)
		req = httptest.NewRequest(http.MethodPost, "/v1/auth/device/requests/"+code+"/approve", reader)
		req.Header.Set("Content-Type", "application/json")
	}
	addCookies(req, f.cookies)
	req.Header.Set(auth.CSRFHeaderName, csrfCookie(f.t, f.cookies))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != want {
		f.t.Fatalf("approve %q status=%d want=%d body=%s", body, rec.Code, want, rec.Body.String())
	}
	if want != http.StatusOK {
		return nil
	}
	return dataOf(f.t, rec.Body.Bytes())
}

func (f deviceEnrollmentFixture) poll(deviceCode string) map[string]any {
	f.t.Helper()
	raw, _ := json.Marshal(map[string]string{"client_id": "paperboat", "device_code": deviceCode})
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/device/token", bytes.NewReader(raw))
	req.RemoteAddr = "198.51.100.21:1234"
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("poll status=%d body=%s", rec.Code, rec.Body.String())
	}
	return dataOf(f.t, rec.Body.Bytes())
}

func dataOf(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data
}

// Approving a device login that is joining must name a workspace the account may add machines
// to, and the one-shot enrollment the device receives is minted for exactly that workspace.
func TestDeviceLoginEnrollsIntoChosenWorkspace(t *testing.T) {
	store, router := newAuthIntegrationRouter(t)
	cookies := loginCookies(t, router, "workos_enroll:enroll@example.com:Enroll User")
	user := userIDByEmail(t, store, "enroll@example.com")
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ('usr_enroll_other','workos_enroll_other','enroll-other@example.com','active')`)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES('enroll-mine',$1,1),('enroll-theirs','usr_enroll_other',1),('enroll-held','usr_enroll_other',1)`, user)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES('enroll-mine',$1,1,'member',true),('enroll-theirs','usr_enroll_other',1,'member',true),('enroll-held',$1,1,'member',true),('enroll-held','usr_enroll_other',1,'member',true)`, user)
	f := deviceEnrollmentFixture{t: t, router: router, cookies: cookies}

	grant := f.authorize(map[string]any{"enroll": true, "workspace": "enroll-mine"}, http.StatusOK)
	seen := f.request(grant.UserCode)
	if seen["enroll"] != true || seen["requested_workspace"] != "enroll-mine" || seen["workspace_id"] != "" {
		t.Fatalf("joining device request=%v", seen)
	}
	f.approve(grant.UserCode, "", http.StatusBadRequest)
	f.approve(grant.UserCode, `{"workspace_id":"enroll-theirs"}`, http.StatusForbidden)
	f.approve(grant.UserCode, `{"workspace_id":"no-such-team"}`, http.StatusForbidden)
	if approved := f.approve(grant.UserCode, `{"workspace_id":"enroll-mine"}`, http.StatusOK); approved["workspace_id"] != "enroll-mine" {
		t.Fatalf("approved request=%v", approved)
	}
	tokens := f.poll(grant.DeviceCode)
	enrollment, _ := tokens["enrollment"].(map[string]any)
	if enrollment == nil || enrollment["bootstrap_token"] == "" || enrollment["workspace_id"] != "enroll-mine" {
		t.Fatalf("token response enrollment=%v", tokens["enrollment"])
	}
	// A lost response is recovered by polling again: same session, same one-shot enrollment.
	again, _ := f.poll(grant.DeviceCode)["enrollment"].(map[string]any)
	if again == nil || again["bootstrap_token"] != enrollment["bootstrap_token"] {
		t.Fatalf("recovered enrollment=%v want=%v", again, enrollment)
	}

	// A team that approves new machines holds a member's computer; the page and the device are told.
	held := f.authorize(map[string]any{"enroll": true, "workspace": "enroll-held"}, http.StatusOK)
	if approved := f.approve(held.UserCode, `{"workspace_id":"enroll-held"}`, http.StatusOK); approved["admission"] != "pending" {
		t.Fatalf("approval page admission=%v", approved["admission"])
	}
	if got, _ := f.poll(held.DeviceCode)["enrollment"].(map[string]any); got == nil || got["admission"] != "pending" {
		t.Fatalf("held enrollment=%v", got)
	}
	if enrollment["admission"] != "accepted" {
		t.Fatalf("an owner's enrollment must be accepted at once: %v", enrollment["admission"])
	}

	personal := f.authorize(map[string]any{"enroll": true}, http.StatusOK)
	f.approve(personal.UserCode, `{"workspace_id":"personal"}`, http.StatusOK)
	if got, _ := f.poll(personal.DeviceCode)["enrollment"].(map[string]any); got == nil || got["workspace_id"] != "personal" {
		t.Fatalf("personal enrollment=%v", got)
	}

	plain := f.authorize(map[string]any{}, http.StatusOK)
	f.approve(plain.UserCode, `{"workspace_id":"personal"}`, http.StatusBadRequest)
	f.approve(plain.UserCode, "", http.StatusOK)
	if got := f.poll(plain.DeviceCode); got["enrollment"] != nil {
		t.Fatalf("a sign-in only login received an enrollment: %v", got["enrollment"])
	}

	f.authorize(map[string]any{"workspace": "enroll-mine"}, http.StatusBadRequest)
	f.authorize(map[string]any{"enroll": true, "enrolled_machine_id": "machine_x"}, http.StatusBadRequest)
}

// A device that already is one of the account's machines is shown where it lives and only signs in.
func TestDeviceLoginShowsEnrolledMachineWorkspace(t *testing.T) {
	store, router := newAuthIntegrationRouter(t)
	cookies := loginCookies(t, router, "workos_enrolled:enrolled@example.com:Enrolled User")
	user := userIDByEmail(t, store, "enrolled@example.com")
	if _, err := store.SQL().Exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES('machine_enrolled_login',$1,'env_enrolled_login','studio','linux','amd64','/home/dev','offline','released')`, user); err != nil {
		t.Fatal(err)
	}
	f := deviceEnrollmentFixture{t: t, router: router, cookies: cookies}
	grant := f.authorize(map[string]any{"enrolled_machine_id": "machine_enrolled_login"}, http.StatusOK)
	machine, _ := f.request(grant.UserCode)["enrolled_machine"].(map[string]any)
	if machine == nil || machine["alias"] != "studio" || machine["id"] != "machine_enrolled_login" {
		t.Fatalf("enrolled machine=%v", machine)
	}
	other := f.authorize(map[string]any{"enrolled_machine_id": "machine_someone_else"}, http.StatusOK)
	if f.request(other.UserCode)["enrolled_machine"] != nil {
		t.Fatal("a machine id the account does not own was described")
	}
}

// A login on a paired machine is that machine's login, so removing or revoking the machine ends it (C209).
// A machine that is already revoked, or not the account's, leaves an account login as on an unpaired computer.
func TestDeviceLoginOnPairedMachineBindsItsSession(t *testing.T) {
	store, router := newAuthIntegrationRouter(t)
	cookies := loginCookies(t, router, "workos_bound:bound@example.com:Bound User")
	user := userIDByEmail(t, store, "bound@example.com")
	if _, err := store.SQL().Exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,revoked_at) VALUES
		('machine_bound_live',$1,'env_bound_live','live','linux','amd64','/home/dev','offline','released',NULL),
		('machine_bound_gone',$1,'env_bound_gone','gone','linux','amd64','/home/dev','revoked','released',now())`, user); err != nil {
		t.Fatal(err)
	}
	f := deviceEnrollmentFixture{t: t, router: router, cookies: cookies}
	boundTo := func(machine string) string {
		t.Helper()
		grant := f.authorize(map[string]any{"enrolled_machine_id": machine}, http.StatusOK)
		f.approve(grant.UserCode, "", http.StatusOK)
		f.poll(grant.DeviceCode)
		var bound *string
		if err := store.SQL().QueryRow(`SELECT user_machine_id FROM paperboat.cli_client_sessions WHERE user_id=$1 ORDER BY created_at DESC LIMIT 1`, user).Scan(&bound); err != nil {
			t.Fatal(err)
		}
		if bound == nil {
			return ""
		}
		return *bound
	}
	if got := boundTo("machine_bound_live"); got != "machine_bound_live" {
		t.Fatalf("login on a paired machine bound to %q", got)
	}
	if got := boundTo("machine_bound_gone"); got != "" {
		t.Fatalf("login on a revoked machine bound to %q", got)
	}
	if got := boundTo("machine_someone_else"); got != "" {
		t.Fatalf("login naming a foreign machine bound to %q", got)
	}
}
