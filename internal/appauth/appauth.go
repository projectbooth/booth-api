// Package appauth verifies the callers of booth-api's app write routes (/app/v1/, ADR 0117
// "Reconciled design" item 2): a booth-streamlit app's own viewer-capped workload token, forwarded
// unchanged by the app's Streamlit gate through core's gateway. Nothing else is accepted there.
//
// The token is verified against booth-core's workload issuer (oidc.workloadIssuerUrl, discovered
// like any OIDC issuer: core serves /.well-known/openid-configuration and its JWKS), never the
// people's OIDC issuer. The workspace comes from the token, never from a header: the subject's own
// `streamlit:<workspace>:<appId>` and the single workspace group core writes into a workload token
// must agree, and core's `booth_module` claim must say booth-streamlit minted it. The principal a
// write grant names is that subject, verbatim.
//
// The reverse holds too: the people's verifier (internal/auth) trusts only the OIDC issuer, so an
// app token is refused on every management route, and the public route takes API keys only.
//
// One more token shape is recognised, and only by the management API's revoke-principal route
// (ADR 0117 item 1a): booth-streamlit's own grants service token, subject
// `streamlit:<workspace>:grants`, which it mints for itself on take-ownership. It may revoke app
// grants in its workspace and do nothing else (VerifyService, internal/api).
package appauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"

	oidc "github.com/coreos/go-oidc/v3/oidc"
)

// AppModule is the module id whose workload tokens are app tokens.
const AppModule = "streamlit"

// ModuleClaim is core's claim naming the module that minted a workload token.
const ModuleClaim = "booth_module"

var (
	// subjectRE is booth-streamlit's subject shape: an app, or one of its reservedAppIDs.
	subjectRE = regexp.MustCompile(`^streamlit:([a-z0-9-]+):([A-Za-z0-9._-]{1,128})$`)
	groupRE   = regexp.MustCompile(`^/workspaces/([a-z0-9-]+)/(owner|editor|viewer)$`)
)

// ErrNotAnApp: a valid token that isn't a booth-streamlit app's.
var ErrNotAnApp = errors.New("not a booth-streamlit app token")

// ErrNotGrantsService: a valid token that isn't booth-streamlit's grants service token.
var ErrNotGrantsService = errors.New("not booth-streamlit's grants service token")

// reservedAppIDs are booth-streamlit's own identities in a workspace, never apps: "sources" lists
// the catalog for its editor, "grants" revokes app grants on take-ownership.
var reservedAppIDs = map[string]bool{"sources": true, "grants": true}

// GrantsServiceID is the appId part of booth-streamlit's grants service subject.
const GrantsServiceID = "grants"

// Service is booth-streamlit's verified grants service token: it may revoke app grants in
// Workspace, nothing else.
type Service struct {
	Subject   string
	Workspace string
}

// ServiceVerifier verifies the grants service token. *Verifier is the production implementation.
type ServiceVerifier interface {
	VerifyService(ctx context.Context, rawToken string) (Service, error)
}

// Caller is a verified app.
type Caller struct {
	// Subject is the token's subject, streamlit:<workspace>:<appId>: the grant principal.
	Subject   string
	Workspace string
	AppID     string
	Role      string
}

// TokenVerifier verifies an app token. *Verifier is the production implementation.
type TokenVerifier interface {
	Verify(ctx context.Context, rawToken string) (Caller, error)
}

// Config is the workload issuer to trust.
type Config struct {
	IssuerURL       string
	ClientID        string
	RequireAudience bool
	GroupsClaim     string
}

// Verifier verifies app tokens against core's workload issuer.
type Verifier struct {
	v           *oidc.IDTokenVerifier
	groupsClaim string
}

// NewVerifier runs discovery against core's workload issuer.
func NewVerifier(ctx context.Context, cfg Config) (*Verifier, error) {
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("discovery against the workload issuer %s: %w", cfg.IssuerURL, err)
	}
	claim := cfg.GroupsClaim
	if claim == "" {
		claim = "groups"
	}
	log.Printf("appauth: verifying app tokens with workload issuer=%s", cfg.IssuerURL)
	return &Verifier{v: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID, SkipClientIDCheck: !cfg.RequireAudience}), groupsClaim: claim}, nil
}

// Verify checks signature, issuer, expiry and (when configured) audience, then the app shape.
func (v *Verifier) Verify(ctx context.Context, rawToken string) (Caller, error) {
	sub, module, groups, err := v.claims(ctx, rawToken)
	if err != nil {
		return Caller{}, err
	}
	return Check(sub, module, groups)
}

// VerifyService checks a token the same way, then the grants service shape.
func (v *Verifier) VerifyService(ctx context.Context, rawToken string) (Service, error) {
	sub, module, groups, err := v.claims(ctx, rawToken)
	if err != nil {
		return Service{}, err
	}
	return CheckService(sub, module, groups)
}

func (v *Verifier) claims(ctx context.Context, rawToken string) (sub, module string, groups []string, err error) {
	tok, err := v.v.Verify(ctx, rawToken)
	if err != nil {
		return "", "", nil, fmt.Errorf("token verification failed: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := tok.Claims(&raw); err != nil {
		return "", "", nil, fmt.Errorf("reading token claims: %w", err)
	}
	_ = json.Unmarshal(raw[ModuleClaim], &module)
	_ = json.Unmarshal(raw[v.groupsClaim], &groups) // the wrong shape is "no groups": refused by the checks
	return tok.Subject, module, groups, nil
}

// CheckService applies the grants service rules: subject exactly streamlit:<workspace>:grants,
// minted by booth-streamlit, with core's single workspace group for that same workspace.
func CheckService(subject, module string, groups []string) (Service, error) {
	m := subjectRE.FindStringSubmatch(subject)
	if m == nil || m[2] != GrantsServiceID || module != AppModule || len(groups) != 1 {
		return Service{}, ErrNotGrantsService
	}
	g := groupRE.FindStringSubmatch(groups[0])
	if g == nil || g[1] != m[1] {
		return Service{}, ErrNotGrantsService
	}
	return Service{Subject: subject, Workspace: m[1]}, nil
}

// Check applies the app-token rules to a verified token's subject, module claim and groups.
func Check(subject, module string, groups []string) (Caller, error) {
	m := subjectRE.FindStringSubmatch(subject)
	if m == nil || reservedAppIDs[m[2]] || module != AppModule {
		return Caller{}, ErrNotAnApp
	}
	// Core writes exactly one group into a workload token: the workspace it was minted for.
	if len(groups) != 1 {
		return Caller{}, ErrNotAnApp
	}
	g := groupRE.FindStringSubmatch(groups[0])
	if g == nil || g[1] != m[1] {
		return Caller{}, ErrNotAnApp
	}
	return Caller{Subject: subject, Workspace: m[1], AppID: m[2], Role: g[2]}, nil
}

// ValidPrincipal reports whether p is an app principal in workspace (what a grant may name).
func ValidPrincipal(p, workspace string) bool {
	m := subjectRE.FindStringSubmatch(p)
	return m != nil && m[1] == workspace && !reservedAppIDs[m[2]]
}
