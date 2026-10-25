// Package nbapi is a thin JSON client for the NetBird management API, built on
// the REST client of the pinned NetBird module.
package nbapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/netbirdio/netbird/shared/management/client/rest"
)

// PinnedVersion is the NetBird version nb-nix is built against. It must equal
// the github.com/netbirdio/netbird requirement in go.mod (checked by a test).
const PinnedVersion = "0.81.4"

// Client sends JSON requests to one management server.
type Client struct {
	rc *rest.Client
}

// New returns a client for the management server at baseURL (for example
// https://netbird.example.com) authenticated with a personal access token.
func New(baseURL, token string) *Client {
	return &Client{rc: rest.NewWithOptions(
		rest.WithManagementURL(strings.TrimRight(baseURL, "/")),
		rest.WithPAT(token),
		rest.WithUserAgent("nb-nix"),
	)}
}

// REST returns the underlying typed REST client.
func (c *Client) REST() *rest.Client { return c.rc }

// Do sends a request with an optional JSON body to path (relative to /api)
// and decodes the JSON response into out, when out is not nil.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%s %s: encode body: %w", method, path, err)
		}
		r = bytes.NewReader(b)
	}
	resp, err := c.rc.NewRequest(ctx, method, "/api"+path, r, nil)
	if err != nil {
		var apiErr *rest.APIError
		if errors.As(err, &apiErr) {
			return &HTTPError{Method: method, Path: path, Status: apiErr.StatusCode, Message: apiErr.Message}
		}
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s %s: read body: %w", method, path, err)
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode body: %w", method, path, err)
	}
	return nil
}

// HTTPError is a non-2xx API response.
type HTTPError struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Message)
}

// ServerVersion returns the running management version without a leading "v".
func (c *Client) ServerVersion(ctx context.Context) (string, error) {
	var v struct {
		Current string `json:"management_current_version"`
	}
	if err := c.Do(ctx, "GET", "/instance/version", nil, &v); err != nil {
		return "", err
	}
	return strings.TrimPrefix(v.Current, "v"), nil
}

// CheckVersion fails unless the server runs the pinned NetBird version.
func (c *Client) CheckVersion(ctx context.Context) error {
	got, err := c.ServerVersion(ctx)
	if err != nil {
		return err
	}
	if got != PinnedVersion {
		return fmt.Errorf("server runs NetBird %s, nb-nix is built for %s (use --ignore-version to override)", got, PinnedVersion)
	}
	return nil
}
