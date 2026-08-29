package tinkeraccess

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchUsers(t *testing.T) {
	const path = "/api/get_users11102523982452806591"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("content-type = %q", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":1,"name":"Jane Doe","code":"12345","status":"A"},
			{"id":2,"name":"John Roe","code":null,"status":"I"},
			{"id":3,"name":null,"code":null,"status":"S"}
		]`))
	}))
	defer srv.Close()

	c := New(srv.URL, path)
	users, err := c.FetchUsers(context.Background())
	if err != nil {
		t.Fatalf("FetchUsers: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("got %d users, want 3", len(users))
	}
	if users[0].Name == nil || *users[0].Name != "Jane Doe" || users[0].Status != "A" {
		t.Errorf("user0 = %+v", users[0])
	}
	if users[1].Code != nil {
		t.Errorf("user1.Code = %v, want nil", *users[1].Code)
	}
	if users[2].Name != nil {
		t.Errorf("user2.Name = %v, want nil", *users[2].Name)
	}
}

func TestFetchUsers404IsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := New(srv.URL, "/api/get_users_stale_hash")
	_, err := c.FetchUsers(context.Background())
	if !errors.Is(err, ErrEndpointNotFound) {
		t.Fatalf("err = %v, want ErrEndpointNotFound", err)
	}
}

func TestFetchUsersServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL, "/api/get_users123456")
	if _, err := c.FetchUsers(context.Background()); err == nil {
		t.Fatal("expected error on 500")
	}
}
