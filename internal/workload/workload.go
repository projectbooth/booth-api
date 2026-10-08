// Package workload mints booth-api's workload tokens (ADR 0056/0058, used per ADR 0103): one per
// workspace and key creator, `subject: apikeys:<workspace>`, `roleCeiling: viewer`, `owner:` the
// key's creator. Core caps the role at what the owner currently holds in that workspace and
// refuses outright if they hold nothing there or haven't signed in within its recency window
// (core's internal/workload/service.go, ownerRole), which is what bounds a key by its creator.
package workload

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

	"github.com/projectbooth/booth-api/internal/source"
)

// RoleCeiling is the most a booth-api workload token may carry: a read lease needs no more.
const RoleCeiling = "viewer"

// Subject names the run the token is for: every key-driven read in a workspace.
func Subject(workspace string) string { return "apikeys:" + workspace }

// Token is a minted workload token.
type Token struct {
	JWT       string
	ExpiresAt time.Time
	Role      string
}

// ErrNotEntitled: core refused booth-api itself (the manifest doesn't declare
// workloadIdentity.mint, or the delivered credential is stale). A deployment problem, not the key's.
var ErrNotEntitled = errors.New("booth-core refused to mint for booth-api: check the manifest's workloadIdentity and the booth-workload-minting-credentials Secret")

// Minter calls core's minting endpoint with the credential core delivered as the
// booth-workload-minting-credentials Secret (keys `url` and `credential`).
type Minter struct {
	URL        string
	Credential string
	HTTP       *http.Client
}

type mintRequest struct {
	Workspace   string `json:"workspace"`
	Subject     string `json:"subject"`
	RoleCeiling string `json:"roleCeiling"`
	Owner       string `json:"owner"`
}

type mintResponse struct {
	Token     string    `json:"token"`
	TokenType string    `json:"tokenType"`
	ExpiresAt time.Time `json:"expiresAt"`
	Role      string    `json:"role"`
}

// Mint returns a token for workspace owned by owner. source.ErrOwnerNoAccess when core refuses the
// owner; ErrNotEntitled when it refuses booth-api.
func (m *Minter) Mint(ctx context.Context, workspace, owner string) (Token, error) {
	body, _ := json.Marshal(mintRequest{Workspace: workspace, Subject: Subject(workspace), RoleCeiling: RoleCeiling, Owner: owner})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.URL, bytes.NewReader(body))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Authorization", "Bearer "+m.Credential)
	req.Header.Set("Content-Type", "application/json")
	hc := m.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("calling booth-core to mint a workload token: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden:
		// Core answers 403 both for "the owner has no current access" and for "this module isn't
		// entitled"; its message says which (core's workload.ErrOwnerNoAccess / ErrNotEntitled).
		if strings.Contains(string(raw), "owner") {
			return Token{}, source.ErrOwnerNoAccess
		}
		return Token{}, fmt.Errorf("%w (%s)", ErrNotEntitled, strings.TrimSpace(string(raw)))
	case http.StatusUnauthorized:
		return Token{}, ErrNotEntitled
	default:
		return Token{}, fmt.Errorf("booth-core answered %d minting a workload token: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out mintResponse
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" || out.ExpiresAt.IsZero() {
		return Token{}, fmt.Errorf("booth-core's mint response is malformed")
	}
	// Core grants the lesser of the ceiling and the owner's role, and viewer is the lowest role, so
	// anything else would be a core bug. Refuse rather than hold a token wider than asked for.
	if out.Role != RoleCeiling {
		return Token{}, fmt.Errorf("booth-core granted role %q above the %q ceiling; refusing the token", out.Role, RoleCeiling)
	}
	return Token{JWT: out.Token, ExpiresAt: out.ExpiresAt, Role: out.Role}, nil
}
