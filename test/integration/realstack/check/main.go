// Command check is the real-stack integration test's in-cluster half: run as a Job beside a real
// Keycloak, booth-core, booth-database, booth-catalog and booth-api (deploy.sh), it drives the whole
// path a person and an API-key holder take, through core's gateway and public route only:
//
//  1. people sign in (Keycloak password grant) and core records their roles;
//  2. tables are created in each workspace's booth-database database through core's broker;
//  3. the tables are registered in booth-catalog as format "postgres" datasets (ADR 0102);
//  4. booth-api generates an API from each dataset (catalog through the gateway, a workload token
//     minted for the person, a real credential sidecar, introspection);
//  5. keys are issued, then used on /modules/api/public/v1/... (ADR 0101): scope, workspace
//     isolation, forged X-Booth-* headers, GraphQL and REST, revocation;
//  6. a key's creator loses access in Keycloak and signs in again: their key is refused (ADR 0103).
//
// Every step prints what it checked; the first failure exits non-zero with what was expected and
// what came back.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	coreURL     = env("CORE_URL", "http://booth-core.booth-system.svc:8080")
	keycloakURL = env("KEYCLOAK_URL", "http://keycloak.keycloak.svc:8080")
	password    = os.Getenv("TEST_PASSWORD")
	adminPass   = os.Getenv("KEYCLOAK_ADMIN_PASSWORD")
	httpc       = &http.Client{Timeout: 60 * time.Second}
)

func main() {
	ctx := context.Background()
	must(password != "" && adminPass != "", "TEST_PASSWORD and KEYCLOAK_ADMIN_PASSWORD are required")

	step("1. sign in, and let core record each person's roles")
	alice, bob, dave, carol := login("alice"), login("bob"), login("dave"), login("carol")
	for _, t := range []string{alice, bob, dave, carol} {
		expect(call("GET", "/api/me", t, "", nil, nil), 200, "GET /api/me")
	}

	step("2. tables in each workspace's own database, through core's credential broker")
	seed(ctx, alice, "acme", `
		CREATE TABLE IF NOT EXISTS orders (id bigint PRIMARY KEY, region text NOT NULL, amount numeric(12,2));
		INSERT INTO orders VALUES (1, 'emea', 12.50), (2, 'apac', 3.00), (9007199254740993, 'amer', 100.01) ON CONFLICT DO NOTHING;
		CREATE TABLE IF NOT EXISTS salaries (id int PRIMARY KEY, person text, amount numeric);
		INSERT INTO salaries VALUES (1, 'secret', 1) ON CONFLICT DO NOTHING;`)
	seed(ctx, carol, "globex", `
		CREATE TABLE IF NOT EXISTS notes (id int PRIMARY KEY, body text);
		INSERT INTO notes VALUES (1, 'globex only') ON CONFLICT DO NOTHING;`)

	step("3. register them in booth-catalog as format: postgres datasets (ADR 0102)")
	orders := dataset(alice, "acme", "Orders", "orders", []map[string]string{{"name": "id", "type": "bigint"}, {"name": "region", "type": "text"}, {"name": "amount", "type": "numeric", "description": "in CAD"}})
	salaries := dataset(alice, "acme", "Salaries", "salaries", nil)
	notes := dataset(carol, "globex", "Notes", "notes", nil)

	step("4. booth-api generates an API from each (catalog via gateway, minted token, real sidecar, introspection)")
	ordersAPI := generate(alice, "acme", orders)
	must(ordersAPI["slug"] == "orders", "slug = %v", ordersAPI["slug"])
	cols := fmt.Sprint(ordersAPI["columns"])
	must(strings.Contains(cols, "numeric(12,2)") && strings.Contains(cols, "in CAD"), "columns = %s (types from the table, descriptions from the catalog)", cols)
	generate(alice, "acme", salaries)
	generate(carol, "globex", notes)
	// A viewer-level call can list, a person in another workspace can't see acme's APIs.
	var list struct{ Items []map[string]any }
	expect(call("GET", "/modules/api/api/apis", carol, "globex", nil, &list), 200, "carol lists APIs")
	must(len(list.Items) == 1 && list.Items[0]["slug"] == "notes", "globex sees %v", list.Items)

	step("5. keys on the public route (/modules/api/public/v1/..., ADR 0101)")
	k1 := issue(alice, "acme", "orders only", orders)
	k3 := issue(carol, "globex", "notes", notes)
	kd := issue(dave, "acme", "dave's", orders)
	kb := issue(bob, "acme", "bob's", orders) // used only after bob loses access (6)

	var rows struct {
		Data []map[string]any
		Page map[string]any
	}
	expect(public("GET", "orders/rows", k1, "", nil, &rows), 200, "k1 reads orders")
	must(len(rows.Data) == 3 && fmt.Sprint(rows.Data[2]["id"]) == "9007199254740993" && rows.Data[0]["amount"] == "12.50",
		"rows = %v (bigint and numeric exact, as strings)", rows.Data)
	expect(public("GET", "orders/rows?filter[region]=apac&fields=id", k1, "", nil, &rows), 200, "k1 filters")
	must(len(rows.Data) == 1 && rows.Data[0]["id"] == "2", "filtered = %v", rows.Data)
	var gq map[string]any
	expect(public("POST", "orders/graphql", k1, `{"query":"{ rows(where: {amount: {gt: \"10\"}}) { nodes { id region } } }"}`, nil, &gq), 200, "k1 GraphQL")
	must(strings.Contains(mustJSON(gq), `"nodes":[{"id":"9007199254740993","region":"amer"},{"id":"1","region":"emea"}]`) ||
		strings.Contains(mustJSON(gq), `"nodes":[{"id":"1","region":"emea"},{"id":"9007199254740993","region":"amer"}]`), "graphql = %s", mustJSON(gq))
	var doc map[string]any
	expect(public("GET", "orders/openapi.json", k1, "", nil, &doc), 200, "k1 OpenAPI document")
	must(doc["openapi"] == "3.1.0", "openapi = %v", doc["openapi"])

	expect(public("GET", "salaries/rows", k1, "", nil, nil), 403, "k1 outside its scope (salaries)")
	expect(public("GET", "orders/rows", "", "", nil, nil), 401, "no key")
	expect(public("GET", "orders/rows", "booth_ak_aaaaaaaaaaaaa_"+strings.Repeat("A", 43), "", nil, nil), 401, "unknown key")
	expect(public("GET", "orders/rows", alice, "", nil, nil), 401, "a platform token is not an API key")
	expect(public("GET", "orders/rows", k3, "", nil, nil), 404, "globex key on acme's API")
	expect(public("GET", "notes/rows", k1, "", nil, nil), 404, "acme key on globex's API")
	expect(public("GET", "orders/rows", k3, "", map[string]string{"X-Booth-Workspace": "acme", "X-Booth-Role": "owner"}, nil), 404,
		"globex key with forged X-Booth-Workspace: acme")
	expect(public("GET", "notes/rows", k3, "", nil, &rows), 200, "k3 reads globex notes")
	must(len(rows.Data) == 1 && rows.Data[0]["body"] == "globex only", "notes = %v", rows.Data)
	expect(public("GET", "orders/rows", kd, "", nil, nil), 200, "an editor's key works")
	// Core refuses a traversal out of the declared prefix before booth-api sees it.
	expect(callRaw("GET", coreURL+"/modules/api/public/v1/../../api/keys", map[string]string{"Authorization": "Bearer " + k1}), 404, "traversal out of /v1/")

	step("5b. revocation takes effect on the next request")
	var keys struct{ Items []map[string]any }
	expect(call("GET", "/modules/api/api/keys", alice, "acme", nil, &keys), 200, "list keys")
	k1id := ""
	for _, k := range keys.Items {
		if strings.Contains(k1, fmt.Sprint(k["id"])) {
			k1id = fmt.Sprint(k["id"])
			must(k["lastUsedAt"] != nil, "k1's lastUsedAt wasn't recorded")
		}
		_, leaked := k["secret"]
		must(!leaked, "the key list includes a secret")
	}
	must(k1id != "", "k1 not in the key list")
	expect(call("POST", "/modules/api/api/keys/"+k1id+"/revoke", alice, "acme", nil, nil), 200, "revoke k1")
	expect(public("GET", "orders/rows", k1, "", nil, nil), 401, "revoked k1")
	expect(public("GET", "orders/rows", kd, "", nil, nil), 200, "dave's key is unaffected")

	step("6. a key's creator loses access: their key is refused (ADR 0103)")
	removeFromGroup("bob", "/workspaces/acme/editor")
	bob = login("bob") // core replaces bob's recorded roles from this token: no acme membership
	expect(call("GET", "/api/me", bob, "", nil, nil), 200, "bob signs in again")
	resp := public("GET", "orders/rows", kb, "", nil, nil)
	expect(resp, 403, "bob's key after bob lost access")
	must(strings.Contains(resp.body, "creator no longer has access"), "refusal = %s", resp.body)
	expect(public("GET", "orders/rows", kd, "", nil, nil), 200, "dave's key still works")

	fmt.Println("\nreal-stack check: OK")
}

// ---- helpers ------------------------------------------------------------------------------------

type response struct {
	code int
	body string
}

func step(s string) { fmt.Println("\n--- " + s) }

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func must(ok bool, format string, args ...any) {
	if !ok {
		fmt.Printf("FAIL: "+format+"\n", args...)
		os.Exit(1)
	}
}

func expect(r response, code int, what string) {
	must(r.code == code, "%s: got %d, want %d: %.400s", what, r.code, code, r.body)
	fmt.Printf("ok   %s (%d)\n", what, code)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// call hits core (its own /api, or a module through the gateway) as a person.
func call(method, path, token, workspace string, body any, out any) response {
	h := map[string]string{"Authorization": "Bearer " + token}
	if workspace != "" {
		h["X-Workspace"] = workspace
	}
	return do(method, coreURL+path, h, body, out)
}

// public hits booth-api's public route through core, with an API key (or nothing).
func public(method, path, key, body string, extra map[string]string, out any) response {
	h := map[string]string{}
	if key != "" {
		h["Authorization"] = "Bearer " + key
	}
	for k, v := range extra {
		h[k] = v
	}
	var b any
	if body != "" {
		b = json.RawMessage(body)
	}
	return do(method, coreURL+"/modules/api/public/v1/"+path, h, b, out)
}

func callRaw(method, rawURL string, h map[string]string) response {
	req, _ := http.NewRequest(method, rawURL, nil)
	req.URL.Opaque = strings.TrimPrefix(rawURL, coreURL) // send the path exactly as written (no client-side cleaning)
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := httpc.Do(req)
	must(err == nil, "%s %s: %v", method, rawURL, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return response{resp.StatusCode, string(b)}
}

func do(method, u string, h map[string]string, body any, out any) response {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, rd)
	must(err == nil, "%v", err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := httpc.Do(req)
	must(err == nil, "%s %s: %v", method, u, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 {
		must(json.Unmarshal(b, out) == nil, "%s %s: response isn't JSON: %.300s", method, u, b)
	}
	return response{resp.StatusCode, string(b)}
}

func login(user string) string {
	form := url.Values{"grant_type": {"password"}, "client_id": {"booth-design"}, "username": {user}, "password": {password}, "scope": {"openid"}}
	resp, err := httpc.PostForm(keycloakURL+"/realms/booth/protocol/openid-connect/token", form)
	must(err == nil, "login %s: %v", user, err)
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	b, _ := io.ReadAll(resp.Body)
	must(resp.StatusCode == 200 && json.Unmarshal(b, &tok) == nil && tok.AccessToken != "", "login %s: %d %s", user, resp.StatusCode, b)
	return tok.AccessToken
}

// seed creates tables with a readwrite credential from core's broker (booth-database's provider).
func seed(ctx context.Context, token, workspace, sql string) {
	var lease struct {
		Credential struct {
			Host, Database, Username, Password, SSLMode string
			Port                                        int
		}
	}
	expect(call("POST", "/api/credentials", token, workspace, map[string]any{"kind": "postgres", "access": "readwrite", "ttlSeconds": 300, "scope": map[string]any{}}, &lease),
		201, "broker issues a readwrite postgres credential for "+workspace)
	c := lease.Credential
	dsn := fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=%s", c.Host, c.Port, c.Database, c.Username, c.Password, c.SSLMode)
	conn, err := pgx.Connect(ctx, dsn)
	must(err == nil, "connecting to %s's database: %v", workspace, err)
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, sql)
	must(err == nil, "seeding %s: %v", workspace, err)
	fmt.Printf("ok   seeded %s's database\n", workspace)
}

func dataset(token, workspace, name, table string, schema []map[string]string) string {
	if schema == nil {
		schema = []map[string]string{}
	}
	var out map[string]any
	expect(call("POST", "/modules/catalog/api/datasets", token, workspace, map[string]any{
		"name": name, "description": "real-stack test", "format": "postgres",
		"postgresTable": map[string]string{"schema": "public", "name": table}, "schema": schema, "tags": []string{},
	}, &out), 201, "catalog registers "+name)
	return fmt.Sprint(out["id"])
}

func generate(token, workspace, datasetID string) map[string]any {
	var out map[string]any
	expect(call("POST", "/modules/api/api/apis", token, workspace, map[string]string{"datasetId": datasetID}, &out), 201, "booth-api generates an API for "+datasetID)
	return out
}

func issue(token, workspace, name, datasetID string) string {
	var out map[string]any
	expect(call("POST", "/modules/api/api/keys", token, workspace, map[string]any{"name": name, "datasetIds": []string{datasetID}}, &out), 201, "issue key "+name)
	s, _ := out["secret"].(string)
	must(strings.HasPrefix(s, "booth_ak_"), "issued key = %v", out)
	return s
}

// removeFromGroup takes a user out of a group with Keycloak's admin API.
func removeFromGroup(user, groupPath string) {
	form := url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {"admin"}, "password": {adminPass}}
	resp, err := httpc.PostForm(keycloakURL+"/realms/master/protocol/openid-connect/token", form)
	must(err == nil, "admin login: %v", err)
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	must(json.Unmarshal(b, &tok) == nil && tok.AccessToken != "", "admin login: %s", b)
	h := map[string]string{"Authorization": "Bearer " + tok.AccessToken}
	admin := keycloakURL + "/admin/realms/booth"
	var users []map[string]any
	expect(do("GET", admin+"/users?exact=true&username="+user, h, nil, &users), 200, "find "+user)
	must(len(users) == 1, "users = %v", users)
	var group map[string]any
	expect(do("GET", admin+"/group-by-path"+groupPath, h, nil, &group), 200, "find group "+groupPath)
	expect(do("DELETE", fmt.Sprintf("%s/users/%v/groups/%v", admin, users[0]["id"], group["id"]), h, nil, nil), 204, "remove "+user+" from "+groupPath)
}
