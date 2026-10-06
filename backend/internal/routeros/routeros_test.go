package routeros

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "awg" || p != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /rest/ip/route":
			if r.URL.Query().Get("routing-table") != "to-awg" {
				t.Errorf("query = %s", r.URL.RawQuery)
			}
			w.Write([]byte(`[{".id":"*1","dst-address":"0.0.0.0/0","active":true,"distance":1}]`))
		case "PUT /rest/routing/rule":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["place-before"] != "*2" {
				t.Errorf("body = %v", body)
			}
			w.Write([]byte(`{".id":"*9"}`))
		case "DELETE /rest/routing/rule/*9":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":400,"message":"Bad Request","detail":"no such command"}`))
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	c, err := New(srv.URL+"/", "awg", "secret", false)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := c.List(ctx, "ip/route", Item{"routing-table": "to-awg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0]["active"] != "true" || routes[0]["distance"] != "1" || routes[0].ID() != "*1" {
		t.Fatalf("routes = %v", routes)
	}
	id, err := c.Add(ctx, "routing/rule", Item{"place-before": "*2"})
	if err != nil || id != "*9" {
		t.Fatalf("add = %q, %v", id, err)
	}
	if err := c.Remove(ctx, "routing/rule", id); err != nil {
		t.Fatal(err)
	}
	if err := c.Run(ctx, "ip/dns", "nope", nil); err == nil || !strings.Contains(err.Error(), "no such command") {
		t.Fatalf("err = %v", err)
	}

	bad, _ := New(srv.URL, "awg", "wrong", false)
	if _, err := bad.Get(ctx, "system/resource"); err == nil || !strings.Contains(err.Error(), "wrong user or password") {
		t.Fatalf("err = %v", err)
	}
	if _, err := New("ftp://x", "", "", false); err == nil {
		t.Fatal("expected URL error")
	}
}
