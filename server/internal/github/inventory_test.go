package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRepositoryProviderPageAndExactID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user/repos":
			q := r.URL.Query()
			if q.Get("page") != "11" || q.Get("per_page") != "20" || q.Get("sort") != "full_name" || q.Get("visibility") != "private" {
				t.Errorf("provider query=%v", q)
			}
			w.Header().Set("Link", fmt.Sprintf("<http://%s/user/repos?page=12>; rel=\"next\"", r.Host))
			_, _ = w.Write([]byte(`[{"id":1101,"name":"later","private":true,"owner":{"login":"owner"}}]`))
		case "/repositories/1101":
			_, _ = w.Write([]byte(`{"id":1101,"name":"later","private":true,"owner":{"login":"owner"}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := HTTPClient{BaseURL: srv.URL}
	items, next, err := client.ListReposPage(context.Background(), "test", 11, 20, true)
	if err != nil || next != 12 || len(items) != 1 || items[0].ID != "1101" {
		t.Fatalf("page=%v next=%d err=%v", items, next, err)
	}
	item, err := client.GetRepoByID(context.Background(), "test", "1101")
	if err != nil || item.ID != "1101" {
		t.Fatalf("exact=%v err=%v", item, err)
	}
}
