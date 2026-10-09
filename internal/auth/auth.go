// Package auth verifies callers of booth-api's management API (/api/*): the people who generate
// APIs and issue keys from the module's UI, reached through booth-core's gateway with their own
// token. API-key callers of generated endpoints never pass through here (ADR 0101's public routes
// authenticate by key, in internal/keys).
//
// A copy of booth-database's internal/auth, itself a copy of booth-storage's, the fleet's reference
// implementation of ADR 0041: the bearer token is re-verified against the same OIDC provider
// booth-core uses, and the caller's role is re-derived from the token's own groups claim
// (ADR 0025). The forwarded X-Booth-Role header can only narrow that role, and a header claiming
// more than the token grants is rejected with 403. Like booth-database, this trusts the OIDC issuer
// only, never booth-core's workload-token issuer: issuing API keys is something a person does.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"

	oidc "github.com/coreos/go-oidc/v3/oidc"
)

// Header names the gateway forwards to a backing module (ADR 0025).
const (
	HeaderBoothWorkspace = "X-Booth-Workspace"
	HeaderBoothRole      = "X-Booth-Role"
)

// Role is a caller's workspace role (ADR 0025).
type Role string

const (
	RoleOwner  Role = "owner"
	RoleEditor Role = "editor"
	RoleViewer Role = "viewer"
)

// Claims is the subset of a verified token this module cares about.
type Claims struct {
	Subject string
	// Groups is the token's workspace-membership claim (ADR 0025), e.g. "/workspaces/acme/owner".
	Groups []string
	// DisplayName is preferred_username, else email, else the subject: shown next to keys a person
	// issued, so a key list isn't a column of opaque subjects.
	DisplayName string
}

// TokenVerifier verifies a raw bearer token. *Verifier is the production implementation; the
// interface exists so the HTTP layer can be tested without a live provider.
type TokenVerifier interface {
	Verify(ctx context.Context, rawToken string) (*Claims, error)
}

// OIDCConfig is the identity-provider configuration â€” the same shape as booth-core's, so every
// module verifies against the same provider.
type OIDCConfig struct {
	IssuerURL       string
	ClientID        string
	RequireAudience bool
	// GroupsClaim names the claim carrying workspace memberships; must match booth-core's.
	// Empty means "groups".
	GroupsClaim string
	// JWKSURL, if set, is where signing keys are fetched from instead of the issuer's discovery
	// document (ADR 0108). `iss` is still validated exactly against IssuerURL. Empty means
	// discovery, as before.
	JWKSURL string
}

// DefaultGroupsClaim matches booth-core's default (ADR 0025).
const DefaultGroupsClaim = "groups"

// Verifier verifies bearer tokens against booth-core's OIDC provider.
type Verifier struct {
	verifier    *oidc.IDTokenVerifier
	groupsClaim string
}

// NewVerifier prepares token verification against the issuer. Ordinarily it runs OIDC discovery
// and uses the provider's own jwks_uri. If cfg.JWKSURL is set (ADR 0108), discovery is skipped
// entirely and keys are fetched from that URL; `iss` is still validated exactly against
// cfg.IssuerURL. Mirrors booth-core's internal/auth/oidc.go (core PR #6). It logs the issuer and
// where keys come from once, on success; it never logs a token.
func NewVerifier(ctx context.Context, cfg OIDCConfig) (*Verifier, error) {
	if cfg.JWKSURL != "" && cfg.IssuerURL == "" {
		return nil, fmt.Errorf("oidc.jwksUrl is set but oidc.issuerUrl is empty: the issuer is still required to validate `iss`")
	}
	verifierCfg := &oidc.Config{SkipClientIDCheck: !cfg.RequireAudience, ClientID: cfg.ClientID}
	var idv *oidc.IDTokenVerifier
	keysFrom := "discovery (" + cfg.IssuerURL + "/.well-known/openid-configuration)"
	if cfg.JWKSURL != "" {
		idv = oidc.NewVerifier(cfg.IssuerURL, oidc.NewRemoteKeySet(ctx, cfg.JWKSURL), verifierCfg)
		keysFrom = cfg.JWKSURL
	} else {
		provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
		if err != nil {
			return nil, fmt.Errorf("oidc discovery against %s: %w", cfg.IssuerURL, err)
		}
		idv = provider.Verifier(verifierCfg)
	}
	claim := cfg.GroupsClaim
	if claim == "" {
		claim = DefaultGroupsClaim
	}
	log.Printf("oidc: verifying tokens with issuer=%s keys-from=%s", cfg.IssuerURL, keysFrom)
	return &Verifier{verifier: idv, groupsClaim: claim}, nil
}

// Verify checks signature, issuer, expiry and (when configured) audience, then reads the groups
// claim. A token from any other issuer â€” including booth-core's workload issuer â€” fails here.
func (v *Verifier) Verify(ctx context.Context, rawToken string) (*Claims, error) {
	idToken, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return nil, fmt.Errorf("token verification failed: %w", err)
	}
	if idToken.Subject == "" {
		return nil, fmt.Errorf("token missing subject")
	}
	var raw map[string]json.RawMessage
	if err := idToken.Claims(&raw); err != nil {
		return nil, fmt.Errorf("reading token claims: %w", err)
	}
	var groups []string
	if g, ok := raw[v.groupsClaim]; ok {
		// A claim of the wrong shape is "no groups" (fail closed), not an error.
		_ = json.Unmarshal(g, &groups)
	}
	display := idToken.Subject
	for _, name := range []string{"preferred_username", "email"} {
		var v string
		if json.Unmarshal(raw[name], &v) == nil && v != "" {
			display = v
			break
		}
	}
	return &Claims{Subject: idToken.Subject, Groups: groups, DisplayName: display}, nil
}

// groupRE is ADR 0025's workspace-membership group shape.
var groupRE = regexp.MustCompile(`^/workspaces/([a-z0-9-]+)/(owner|editor|viewer)$`)

func rank(r Role) int {
	switch r {
	case RoleOwner:
		return 3
	case RoleEditor:
		return 2
	case RoleViewer:
		return 1
	}
	return 0
}

// RoleInWorkspace returns the highest role the groups grant in workspace, or "" if none.
func RoleInWorkspace(groups []string, workspace string) Role {
	var best Role
	for _, g := range groups {
		m := groupRE.FindStringSubmatch(g)
		if m == nil || m[1] != workspace {
			continue
		}
		if r := Role(m[2]); rank(r) > rank(best) {
			best = r
		}
	}
	return best
}

// EffectiveRole is never stronger than the token's grant, nor than the forwarded role (a gateway
// may narrow, never widen). An absent forwarded role means "use the token's"; an unrecognized one
// yields "" â€” no access.
func EffectiveRole(forwarded, granted Role) Role {
	if forwarded == "" {
		return granted
	}
	if rank(forwarded) == 0 {
		return ""
	}
	if rank(forwarded) < rank(granted) {
		return forwarded
	}
	return granted
}

// Identity is the caller identity attached to a request's context by Middleware.
type Identity struct {
	Subject     string
	DisplayName string
	Workspace   string
	Role        Role
	// Token is the caller's verified bearer token, kept only to call booth-catalog through core's
	// gateway on the caller's behalf (internal/catalog). Never logged or stored.
	Token string
}

// CanWrite reports whether the caller may change this module's state in the active workspace:
// generate or delete an API, issue or revoke a key. Editor and owner write, viewer reads — the
// ADR 0038/0048 precedent.
func (i Identity) CanWrite() bool { return i.Role == RoleOwner || i.Role == RoleEditor }

type contextKey struct{}

// FromContext returns the identity Middleware attached, if any.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(contextKey{}).(Identity)
	return id, ok
}

// WithIdentity attaches an identity the way Middleware does â€” for handler tests.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// Middleware verifies the bearer token, reads the gateway-forwarded workspace, and derives the
// caller's role there from the token itself (ADR 0041).
func Middleware(verifier TokenVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token == "" {
				WriteError(w, http.StatusUnauthorized, "missing bearer token")
				return
			}
			claims, err := verifier.Verify(r.Context(), token)
			if err != nil {
				WriteError(w, http.StatusUnauthorized, "invalid token")
				return
			}
			workspace := r.Header.Get(HeaderBoothWorkspace)
			if workspace == "" {
				WriteError(w, http.StatusBadRequest, "missing "+HeaderBoothWorkspace+" header")
				return
			}
			granted := RoleInWorkspace(claims.Groups, workspace)
			if granted == "" {
				WriteError(w, http.StatusForbidden, "your token grants no role in this workspace")
				return
			}
			forwarded := Role(r.Header.Get(HeaderBoothRole))
			if rank(forwarded) > rank(granted) {
				log.Printf("auth: rejected: forwarded role %q exceeds token-derived role %q for sub=%s workspace=%s", forwarded, granted, claims.Subject, workspace)
				WriteError(w, http.StatusForbidden, "the forwarded role exceeds what your token grants in this workspace")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), Identity{
				Subject: claims.Subject, DisplayName: claims.DisplayName, Workspace: workspace,
				Role: EffectiveRole(forwarded, granted), Token: token,
			})))
		})
	}
}

// WriteError writes the JSON error body used across this module's user-facing API.
func WriteError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
