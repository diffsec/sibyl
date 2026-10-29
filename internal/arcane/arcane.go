// Package arcane signs in to Arcane and changes a password through its API.
// Sibyl uses it to replace Arcane's default admin password on a new install.
package arcane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The account that a new Arcane install creates.
const (
	DefaultUser     = "arcane"
	DefaultPassword = "arcane-admin"
)

// ErrLogin means Arcane refused the user name or password.
var ErrLogin = errors.New("arcane refused the login")

// Client calls the Arcane API.
type Client struct {
	Base string // http://arcane:3552
	HTTP *http.Client
}

// New returns a client with a request timeout.
func New(base string) *Client {
	return &Client{Base: strings.TrimSuffix(base, "/"), HTTP: &http.Client{Timeout: 20 * time.Second}}
}

func (c *Client) post(ctx context.Context, path, token string, in, out any) (int, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/api"+path, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("arcane %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return resp.StatusCode, json.Unmarshal(raw, out)
	}
	return resp.StatusCode, nil
}

// Login signs in and returns a session token. It returns ErrLogin when
// Arcane refuses the user name or password.
func (c *Client) Login(ctx context.Context, user, password string) (string, error) {
	var r struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	code, err := c.post(ctx, "/auth/login", "", map[string]string{"username": user, "password": password}, &r)
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return "", ErrLogin
	case err != nil:
		return "", err
	case r.Data.Token == "":
		return "", errors.New("arcane sent no session token")
	}
	return r.Data.Token, nil
}

// ChangePassword changes the signed-in user's password.
func (c *Client) ChangePassword(ctx context.Context, token, current, next string) error {
	_, err := c.post(ctx, "/auth/password", token, map[string]string{"currentPassword": current, "newPassword": next}, nil)
	return err
}
