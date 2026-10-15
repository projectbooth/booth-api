// Package broker makes booth-api's own one-off calls to booth-core's credential broker
// (contracts/credential-broker.md) for table-scoped writes (ADR 0117): a full-scope `tablewrite`
// request whenever an API's write settings are saved (and once when a write meets SQLSTATE 42501),
// and the revoke request when writes are turned off. Both run on an `apiwrite:<api-id>` workload
// token. The write sidecars make their own lease requests (internal/sidecars); this package only
// keeps booth-database's write group in step with booth-api's settings.
//
// A full-scope request mints a lease booth-api never uses: the response's credential is never
// decoded, kept or logged, and the lease expires on its own (booth-database's reaper drops it).
package broker

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

// Path is the broker's endpoint on booth-core.
const Path = "/api/credentials"

// ErrRefused: the broker refused the caller (401/403): the token's role isn't enough, or its
// subject may not ask for this. ScopeError is the provider refusing the scope itself.
var ErrRefused = errors.New("booth-core's credential broker refused the request")

// ScopeError is the provider's 422 `scope_not_supported`: the message names the first table or
// field it couldn't scope (no values), and is shown to the owner saving the settings.
type ScopeError struct{ Message string }

func (e *ScopeError) Error() string { return "booth-database refused the write scope: " + e.Message }

// Client calls the broker.
type Client struct {
	CoreURL string
	HTTP    *http.Client
}

type request struct {
	Kind       string          `json:"kind"`
	TTLSeconds int             `json:"ttlSeconds"`
	Access     string          `json:"access"`
	Scope      json.RawMessage `json:"scope"`
	Options    json.RawMessage `json:"options,omitempty"`
	OnBehalfOf string          `json:"onBehalfOf,omitempty"`
}

// response is the part of a 201 booth-api reads. The credential is deliberately not a field.
type response struct {
	LeaseID   string    `json:"leaseId"`
	ExpiresAt time.Time `json:"expiresAt"`
	Scope     struct {
		Write struct {
			Grant   string `json:"grant"`
			Revoked bool   `json:"revoked"`
		} `json:"write"`
	} `json:"scope"`
}

// TableWrite sends a full-scope `tablewrite` request: booth-database reissues the grant's write
// group to exactly this scope (narrowing reaches every live lease of the grant at its next
// statement). onBehalfOf is audit only.
func (c *Client) TableWrite(ctx context.Context, token, workspace string, scope json.RawMessage, onBehalfOf string) error {
	_, err := c.call(ctx, token, workspace, request{Kind: "postgres", TTLSeconds: 60, Access: "tablewrite", Scope: scope, OnBehalfOf: onBehalfOf})
	return err
}

// RevokeTableWrite sends the revoke request for a grant: `tables: []` with `options.revoke`.
// booth-database ends every live lease of the grant and drops its write group; the broker relays
// its 201 with `credential: null` and `scope.write.revoked: true`.
func (c *Client) RevokeTableWrite(ctx context.Context, token, workspace, grant, onBehalfOf string) error {
	scope, _ := json.Marshal(map[string]any{"workspace": workspace, "write": map[string]any{"grant": grant, "tables": []any{}}})
	out, err := c.call(ctx, token, workspace, request{
		Kind: "postgres", TTLSeconds: 60, Access: "tablewrite", Scope: scope,
		Options: json.RawMessage(`{"revoke":true}`), OnBehalfOf: onBehalfOf,
	})
	if err != nil {
		return err
	}
	if !out.Scope.Write.Revoked {
		return fmt.Errorf("booth-core answered the revoke request without scope.write.revoked")
	}
	return nil
}

func (c *Client) call(ctx context.Context, token, workspace string, r request) (response, error) {
	body, _ := json.Marshal(r)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.CoreURL, "/")+Path, bytes.NewReader(body))
	if err != nil {
		return response{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Workspace", workspace)
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return response{}, fmt.Errorf("calling booth-core's credential broker: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusCreated:
		var out response
		if err := json.Unmarshal(raw, &out); err != nil {
			return response{}, fmt.Errorf("booth-core's credential broker answered with a malformed body")
		}
		return out, nil
	case resp.StatusCode == http.StatusUnprocessableEntity:
		var e struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Message
		if msg == "" {
			msg = e.Error
		}
		if msg == "" {
			msg = "scope not supported"
		}
		return response{}, &ScopeError{Message: msg}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return response{}, fmt.Errorf("%w (%d)", ErrRefused, resp.StatusCode)
	}
	// Never echo a body that might hold a credential; a non-201 shouldn't, but say only the code.
	return response{}, fmt.Errorf("booth-core's credential broker answered %d", resp.StatusCode)
}
