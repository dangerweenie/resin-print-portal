// Package tinkeraccess talks to the TinkerAccess Rust server's Leptos
// `get_users` server-function endpoint, which returns the member roster with
// each member's activation status (A/I/S). See docs/GET_MEMBERS_ENDPOINT.md.
package tinkeraccess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrEndpointNotFound means the server returned 404 for the configured path —
// the Leptos `#[server]` hash suffix has almost certainly changed. Recover it
// with RecoverUsersPath or the steps in docs/GET_MEMBERS_ENDPOINT.md.
var ErrEndpointNotFound = errors.New("tinkeraccess: get_users endpoint returned 404 (hash suffix changed?)")

// User is one row from get_users. Name and Code may be null upstream.
type User struct {
	ID     int64   `json:"id"`
	Name   *string `json:"name"`
	Code   *string `json:"code"`
	Status string  `json:"status"` // "A", "I", or "S"
}

// Client is a configured caller of the get_users endpoint.
type Client struct {
	baseURL string
	path    string
	http    *http.Client
}

// New builds a client. path is the full request path including the Leptos hash
// suffix, e.g. "/api/get_users11102523982452806591".
func New(baseURL, path string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		path:    "/" + strings.TrimLeft(path, "/"),
		http:    &http.Client{Timeout: 20 * time.Second},
	}
}

// FetchUsers returns the full roster. A non-2xx response is an error and the
// caller must treat the previous roster as still authoritative — never sync a
// failed fetch.
func (c *Client) FetchUsers(ctx context.Context) ([]User, error) {
	return c.fetch(ctx, nil)
}

// FetchUsersByStatus returns only members in the given statuses ("A","I","S").
func (c *Client) FetchUsersByStatus(ctx context.Context, statuses ...string) ([]User, error) {
	form := url.Values{}
	for i, s := range statuses {
		form.Set(fmt.Sprintf("status_filter[%d]", i), s)
	}
	return c.fetch(ctx, form)
}

func (c *Client) fetch(ctx context.Context, form url.Values) ([]User, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+c.path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tinkeraccess: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrEndpointNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("tinkeraccess: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var users []User
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return nil, fmt.Errorf("tinkeraccess: decode: %w", err)
	}
	return users, nil
}
