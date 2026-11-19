package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/announcements"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func TestAnnouncementHandlersWriteReadAndDismissPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() }) // the first cleanup runs last: later cleanups still use store
	ctx := context.Background()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	sfx := fmt.Sprint(time.Now().UnixNano())
	admin, alice := "ann-h-admin-"+sfx, "ann-h-alice-"+sfx
	for _, id := range []string{admin, alice} {
		if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, id); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.announcements WHERE created_by=$1`, admin)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id IN ($1,$2)`, admin, alice)
	})
	service := announcements.New(store)
	as := func(id string, r *http.Request) *http.Request {
		return r.WithContext(context.WithValue(r.Context(), authContextKey{}, principal{User: auth.User{ID: id}}))
	}
	call := func(h http.HandlerFunc, id, method, path, body, key string, path1 string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Idempotency-Key", key)
		if path1 != "" {
			r.SetPathValue("announcement_id", path1)
		}
		w := httptest.NewRecorder()
		h(w, as(id, r))
		return w
	}
	type wrapped struct {
		Data announcements.Announcement `json:"data"`
	}
	body := fmt.Sprintf(`{"kind":"banner","title":"Beta %s","body":"Welcome.","link_url":"https://example.test/changelog","severity":"warning","audience_kind":"user","audience_value":%q}`, sfx, alice)
	w := call(operatorAnnouncementWrite(service, ""), admin, "POST", "/v1/admin/announcements", body, "create-"+sfx, "")
	var created wrapped
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &created) != nil || created.Data.State != "live" {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	// A refusal names what to fix; an unknown field is refused outright.
	w = call(operatorAnnouncementWrite(service, ""), admin, "POST", "/v1/admin/announcements", `{"kind":"banner","title":"","audience_kind":"all"}`, "bad-"+sfx, "")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "title is 1 to 80") {
		t.Fatalf("empty title = %d %s", w.Code, w.Body)
	}
	if w = call(operatorAnnouncementWrite(service, ""), admin, "POST", "/v1/admin/announcements", `{"kind":"banner","title":"x","audience_kind":"all","unknown":1}`, "unk-"+sfx, ""); w.Code != 400 {
		t.Fatalf("unknown field = %d", w.Code)
	}
	if w = call(operatorAnnouncementWrite(service, "announcement_id"), admin, "PUT", "/x", body, "put-"+sfx, "ann_nonexistent"); w.Code != 404 {
		t.Fatalf("edit of nothing = %d %s", w.Code, w.Body)
	}

	w = call(announcementsGet(service), alice, "GET", "/v1/announcements", "", "", "")
	var view struct {
		Data announcements.View `json:"data"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil || view.Data.Banner == nil || view.Data.Banner.ID != created.Data.ID || view.Data.Banner.Severity != "warning" {
		t.Fatalf("alice's view = %d %s", w.Code, w.Body)
	}
	if w = call(announcementsDismiss(service), alice, "POST", "/x", "", "", created.Data.ID); w.Code != 204 {
		t.Fatalf("dismiss = %d %s", w.Code, w.Body)
	}
	if w = call(announcementsDismiss(service), alice, "POST", "/x", "", "", "ann_nonexistent"); w.Code != 404 {
		t.Fatalf("dismiss nothing = %d", w.Code)
	}
	if w = call(announcementsRead(service), alice, "POST", "/v1/announcements/read", `{"ids":[]}`, "", ""); w.Code != 204 {
		t.Fatalf("read = %d %s", w.Code, w.Body)
	}
	w = call(announcementsGet(service), alice, "GET", "/v1/announcements", "", "", "")
	if json.Unmarshal(w.Body.Bytes(), &view) != nil || (view.Data.Banner != nil && view.Data.Banner.ID == created.Data.ID) {
		t.Fatalf("dismissed banner still shown: %s", w.Body)
	}
	if w = call(operatorAnnouncementRetract(service), admin, "POST", "/x", "", "retract-"+sfx, created.Data.ID); w.Code != 200 || !strings.Contains(w.Body.String(), `"dismissals":1`) {
		t.Fatalf("retract = %d %s", w.Code, w.Body)
	}
	var audited int
	store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE resource_type='announcement' AND resource_id=$1`, created.Data.ID).Scan(&audited)
	if audited != 2 {
		t.Fatalf("audit events for the announcement = %d, want create + retract", audited)
	}
}
