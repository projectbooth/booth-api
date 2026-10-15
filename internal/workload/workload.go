// Package workload mints booth-api's workload tokens (ADR 0056/0058). Two kinds:
//
//   - reads (ADR 0103): one per workspace and key creator, `subject: apikeys:<workspace>`,
//     `roleCeiling: viewer`, `owner:` the key's creator;
//   - writes (ADR 0117): one per API and granting owner, `subject: apiwrite:<api-id>`,
//     `roleCeiling: editor`, `owner:` the owner who granted the write. Core reserves the
//     `apiwrite:` subject kind to booth-api's module id, and its broker lets such a token ask for a
//     `tablewrite` lease only.
//
// Core caps the role at what the owner currently holds in that workspace and refuses outright if
// they hold nothing there or haven't signed in within its recency window (core's
// internal/workload/service.go, ownerRole), which is what bounds a key by its creator and a write
// grant by its granting owner.
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

// Role ceilings: a read lease needs no more than viewer; a tablewrite lease needs editor.
const (
	RoleCeiling      = "viewer"
	WriteRoleCeiling = "editor"
)

// Subject names the run a read token is for: every key-driven read in a workspace.
func Subject(workspace string) string { return "apikeys:" + workspace }

// WriteSubject names the run a write token is for: writes to one API (ADR 0117).
func WriteSubject(apiID string) string { return "apiwrite:" + apiID }

// Request is one mint: who the token is for, in which workspace, under whose access.
type Request struct {
	Workspace   string `json:"workspace"`
	Subject     string `json:"subject"`
	RoleCeiling string `json:"roleCeiling"`
	Owner       string `json:"owner"`
}

// ReadRequest is the token a workspace's read sidecar runs on for owner's keys.
func ReadRequest(workspace, owner string) Request {
	return Request{Workspace: workspace, Subject: Subject(workspace), RoleCeiling: RoleCeiling, Owner: owner}
}

// WriteRequest is the token an API's write sidecar (and booth-api's own broker calls for that API)
// runs on for owner's grants.
func WriteRequest(workspace, apiID, owner string) Request {
	return Request{Workspace: workspace, Subject: WriteSubject(apiID), RoleCeiling: WriteRoleCeiling, Owner: owner}
}

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

type mintResponse struct {
	Token     string    `json:"token"`
	TokenType string    `json:"tokenType"`
	ExpiresAt time.Time `json:"expiresAt"`
	Role      string    `json:"role"`
}

// Mint returns the token r asks for. source.ErrOwnerNoAccess when core refuses the owner, or grants
// less than the ceiling a write needs; ErrNotEntitled when it refuses booth-api.
func (m *Minter) Mint(ctx context.Context, r Request) (Token, error) {
	body, _ := json.Marshal(r)
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
	// Core grants the lesser of the ceiling and the owner's role. More than the ceiling would be a
	// core bug: refuse rather than hold a token wider than asked for. Less than the ceiling only
	// matters for a write token: an owner demoted to viewer can no longer back a write, which is
	// the same as having no access for this purpose (the broker would refuse tablewrite anyway).
	if rank(out.Role) > rank(r.RoleCeiling) || rank(out.Role) == 0 {
		return Token{}, fmt.Errorf("booth-core granted role %q for the %q ceiling; refusing the token", out.Role, r.RoleCeiling)
	}
	if out.Role != r.RoleCeiling {
		return Token{}, source.ErrOwnerNoAccess
	}
	return Token{JWT: out.Token, ExpiresAt: out.ExpiresAt, Role: out.Role}, nil
}

func rank(role string) int {
	switch role {
	case "owner":
		return 3
	case "editor":
		return 2
	case "viewer":
		return 1
	}
	return 0
}
