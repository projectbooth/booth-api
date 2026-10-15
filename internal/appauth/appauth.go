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
	// subjectRE is booth-streamlit's app subject. "sources" is booth-streamlit's own catalog
	// listing identity (dataaccess.SourcesSubject), never an app.
	subjectRE = regexp.MustCompile(`^streamlit:([a-z0-9-]+):([A-Za-z0-9._-]{1,128})$`)
	groupRE   = regexp.MustCompile(`^/workspaces/([a-z0-9-]+)/(owner|editor|viewer)$`)
)

// ErrNotAnApp: a valid token that isn't a booth-streamlit app's.
var ErrNotAnApp = errors.New("not a booth-streamlit app token")

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
	tok, err := v.v.Verify(ctx, rawToken)
	if err != nil {
		return Caller{}, fmt.Errorf("token verification failed: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := tok.Claims(&raw); err != nil {
		return Caller{}, fmt.Errorf("reading token claims: %w", err)
	}
	var module string
	_ = json.Unmarshal(raw[ModuleClaim], &module)
	var groups []string
	_ = json.Unmarshal(raw[v.groupsClaim], &groups) // the wrong shape is "no groups": refused below
	return Check(tok.Subject, module, groups)
}

// Check applies the app-token rules to a verified token's subject, module claim and groups.
func Check(subject, module string, groups []string) (Caller, error) {
	m := subjectRE.FindStringSubmatch(subject)
	if m == nil || m[2] == "sources" || module != AppModule {
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
	return m != nil && m[1] == workspace && m[2] != "sources"
}
