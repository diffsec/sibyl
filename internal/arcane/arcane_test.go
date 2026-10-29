package arcane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeArcane has one account, and gives the token "t" to a good login.
type fakeArcane struct{ password string }

func (f *fakeArcane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var b map[string]string
	_ = json.NewDecoder(r.Body).Decode(&b)
	switch r.URL.Path {
	case "/api/auth/login":
		if b["username"] != DefaultUser || b["password"] != f.password {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"data":{"token":"t","user":{"username":"arcane"}}}`))
	case "/api/auth/password":
		if r.Header.Get("Authorization") != "Bearer t" || b["currentPassword"] != f.password || len(b["newPassword"]) < 8 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.password = b["newPassword"]
		_, _ = w.Write([]byte(`{"success":true,"data":{"message":"ok"}}`))
	default:
		http.NotFound(w, r)
	}
}

func TestLoginAndChange(t *testing.T) {
	f := &fakeArcane{password: DefaultPassword}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := New(srv.URL)
	ctx := context.Background()
	tok, err := c.Login(ctx, DefaultUser, DefaultPassword)
	if err != nil || tok != "t" {
		t.Fatalf("login %q %v", tok, err)
	}
	if err := c.ChangePassword(ctx, tok, DefaultPassword, "new-password-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Login(ctx, DefaultUser, DefaultPassword); !errors.Is(err, ErrLogin) {
		t.Errorf("old password: %v, want ErrLogin", err)
	}
	if _, err := c.Login(ctx, DefaultUser, "new-password-1"); err != nil {
		t.Errorf("new password: %v", err)
	}
}
