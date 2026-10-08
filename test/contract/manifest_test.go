// Package contract validates booth-api's own manifest — the BoothModule custom resource its Helm
// chart templates — against contracts/module-manifest.md, plus the chart properties the module
// depends on. Per contracts/testing-strategy.md this runs against rendered templates
// (`helm template`), not a cluster. Skips without helm unless BOOTH_TEST_REQUIRE_HELM is set (CI
// sets it).
package contract

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type boothModule struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Spec       struct {
		ID                  string         `yaml:"id"`
		DisplayName         string         `yaml:"displayName"`
		Version             string         `yaml:"version"`
		ContractVersion     string         `yaml:"contractVersion"`
		HasOwnUI            bool           `yaml:"hasOwnUi"`
		UIIntegrationMode   string         `yaml:"uiIntegrationMode"`
		HealthCheckPath     string         `yaml:"healthCheckPath"`
		NavPath             string         `yaml:"navPath"`
		NavGroup            string         `yaml:"navGroup"`
		AdminNavPath        string         `yaml:"adminNavPath"`
		Database            map[string]any `yaml:"database"`
		WorkloadIdentity    map[string]any `yaml:"workloadIdentity"`
		Events              map[string]any `yaml:"events"`
		ProvidesCredentials map[string]any `yaml:"providesCredentials"`
		PublicRoutes        *struct {
			PathPrefixes []string `yaml:"pathPrefixes"`
		} `yaml:"publicRoutes"`
		ServiceRef struct {
			Name string `yaml:"name"`
			Port int    `yaml:"port"`
		} `yaml:"serviceRef"`
	} `yaml:"spec"`
}

func requireHelm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("BOOTH_TEST_REQUIRE_HELM") != "" {
			t.Fatal("helm not installed but BOOTH_TEST_REQUIRE_HELM is set")
		}
		t.Skip("helm not installed; CI runs this")
	}
}

func runHelm(extra ...string) ([]byte, error) {
	args := append([]string{"template", "a", filepath.Join("..", "..", "charts", "booth-api"), "--namespace", "booth-api"}, extra...)
	return exec.Command("helm", args...).CombinedOutput()
}

func helmTemplate(t *testing.T, extra ...string) []byte {
	t.Helper()
	out, err := runHelm(extra...)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return out
}

// docs splits a multi-document render into generic objects.
func docs(t *testing.T, rendered []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(rendered))
	for {
		var d map[string]any
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("parsing rendered chart: %v", err)
		}
		if d != nil {
			out = append(out, d)
		}
	}
}

func renderBoothModule(t *testing.T) boothModule {
	t.Helper()
	out := helmTemplate(t, "--show-only", "templates/boothmodule.yaml")
	var m boothModule
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatalf("parsing BoothModule: %v\n%s", err, out)
	}
	return m
}

func TestManifest_RequiredFields(t *testing.T) {
	requireHelm(t)
	m := renderBoothModule(t)
	if m.APIVersion != "booth.projectbooth.io/v1alpha1" || m.Kind != "BoothModule" {
		t.Errorf("apiVersion/kind = %s/%s (ADR 0019)", m.APIVersion, m.Kind)
	}
	if m.Spec.ID != "api" { // "the repo name minus booth-"
		t.Errorf("spec.id = %q, want api", m.Spec.ID)
	}
	semver := regexp.MustCompile(`^\d+\.\d+\.\d+`)
	if m.Spec.DisplayName == "" || !semver.MatchString(m.Spec.Version) || !semver.MatchString(m.Spec.ContractVersion) {
		t.Errorf("displayName/version/contractVersion = %q/%q/%q", m.Spec.DisplayName, m.Spec.Version, m.Spec.ContractVersion)
	}
	if m.Spec.HealthCheckPath != "/healthz" {
		t.Errorf("healthCheckPath = %q, want /healthz (the route the server actually serves)", m.Spec.HealthCheckPath)
	}
	if m.Spec.ServiceRef.Name == "" || m.Spec.ServiceRef.Port != 8080 {
		t.Errorf("serviceRef = %+v", m.Spec.ServiceRef)
	}
}

// agent-briefs/api.md: Manage nav group; contracts/ui-integration.md lists booth-api as native.
// navPath must not fall under a prefix the shell proxies to booth-core (/api/, /modules/,
// /iframe/), or the browser would never reach the module's page.
func TestManifest_UI(t *testing.T) {
	requireHelm(t)
	m := renderBoothModule(t)
	if !m.Spec.HasOwnUI || m.Spec.UIIntegrationMode != "native" || m.Spec.NavGroup != "manage" {
		t.Errorf("hasOwnUi=%v mode=%q navGroup=%q, want true/native/manage", m.Spec.HasOwnUI, m.Spec.UIIntegrationMode, m.Spec.NavGroup)
	}
	if !strings.HasPrefix(m.Spec.NavPath, "/") {
		t.Errorf("navPath = %q", m.Spec.NavPath)
	}
	for _, reserved := range []string{"/api/", "/modules/", "/iframe/"} {
		if strings.HasPrefix(m.Spec.NavPath+"/", reserved) {
			t.Errorf("navPath %q falls under %s, which the shell proxies to booth-core", m.Spec.NavPath, reserved)
		}
	}
}

// The module stores API key hashes in its own database (ADR 0053, ADR 0100), so it must ask
// core for one. It publishes no events, mints no workload tokens and provides no credentials.
func TestManifest_Capabilities(t *testing.T) {
	requireHelm(t)
	m := renderBoothModule(t)
	if m.Spec.Database["enabled"] != true {
		t.Errorf("database = %v, want {enabled: true}", m.Spec.Database)
	}
	if m.Spec.Events != nil || m.Spec.WorkloadIdentity != nil || m.Spec.ProvidesCredentials != nil {
		t.Error("manifest declares capabilities this module doesn't use")
	}
}

func TestChart_PodHardening(t *testing.T) {
	requireHelm(t)
	var spec map[string]any
	for _, d := range docs(t, helmTemplate(t)) {
		if d["kind"] == "Deployment" {
			spec = d["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		}
	}
	if spec == nil {
		t.Fatal("no Deployment rendered")
	}
	if spec["automountServiceAccountToken"] != false {
		t.Error("pod mounts a Kubernetes token it has no use for")
	}
	psc := spec["securityContext"].(map[string]any)
	if psc["runAsNonRoot"] != true {
		t.Error("pod may run as root")
	}
	c := spec["containers"].([]any)[0].(map[string]any)
	sc := c["securityContext"].(map[string]any)
	if sc["readOnlyRootFilesystem"] != true || sc["allowPrivilegeEscalation"] != false {
		t.Errorf("container securityContext = %v", sc)
	}
	// The DSN comes from core's provisioned Secret (ADR 0053), never a chart value.
	var fromSecret bool
	for _, e := range c["env"].([]any) {
		ev := e.(map[string]any)
		if ev["name"] == "BOOTH_API_DATABASE_DSN" {
			ref := ev["valueFrom"].(map[string]any)["secretKeyRef"].(map[string]any)
			fromSecret = ref["name"] == "booth-database-credentials" && ref["key"] == "dsn"
		}
	}
	if !fromSecret {
		t.Error("BOOTH_API_DATABASE_DSN is not read from booth-database-credentials/dsn")
	}
}

// The management API verifies tokens against the same OIDC provider core uses (ADR 0041), and reads
// the catalog through core's gateway. Nothing renders unless configured; an issuer without a client
// id fails the render.
func TestChart_OIDCAndCore(t *testing.T) {
	requireHelm(t)
	if s := string(helmTemplate(t)); strings.Contains(s, "BOOTH_OIDC_ISSUER_URL") || strings.Contains(s, "BOOTH_CORE_URL") {
		t.Error("OIDC or core settings rendered without being configured")
	}
	s := string(helmTemplate(t, "--set", "oidc.issuerUrl=https://idp.example/realms/booth", "--set", "oidc.clientId=booth",
		"--set", "core.url=http://booth-core.booth-system.svc:8080"))
	for _, want := range []string{"BOOTH_OIDC_ISSUER_URL", `value: "https://idp.example/realms/booth"`, "BOOTH_OIDC_GROUPS_CLAIM",
		"BOOTH_OIDC_REQUIRE_AUDIENCE", "BOOTH_CORE_URL", `value: "http://booth-core.booth-system.svc:8080"`} {
		if !strings.Contains(s, want) {
			t.Errorf("render lacks %s", want)
		}
	}
	if out, err := runHelm("--set", "oidc.issuerUrl=https://idp.example"); err == nil {
		t.Errorf("rendered an issuer with no client id:\n%.200s", out)
	}
}

// ADR 0101: exactly one public prefix, /v1/, which is where internal/public is mounted. The
// contract's grammar requires a leading and trailing slash.
func TestManifest_PublicRoutes(t *testing.T) {
	requireHelm(t)
	m := renderBoothModule(t)
	if m.Spec.PublicRoutes == nil || strings.Join(m.Spec.PublicRoutes.PathPrefixes, ",") != "/v1/" {
		t.Errorf("publicRoutes = %+v, want {pathPrefixes: [/v1/]}", m.Spec.PublicRoutes)
	}
}

// ADR 0103: workload minting is declared exactly when the data path can work (core.url set), and
// the deployment then reads the minting Secret core delivers and gives the sidecars an in-memory
// directory (the root filesystem is read-only).
func TestChart_DataAccess(t *testing.T) {
	requireHelm(t)
	off := string(helmTemplate(t))
	if strings.Contains(off, "workloadIdentity") || strings.Contains(off, "booth-workload-minting-credentials") {
		t.Error("workload minting rendered without core.url")
	}
	on := helmTemplate(t, "--set", "core.url=http://booth-core.booth-system.svc:8080")
	if !strings.Contains(string(on), "workloadIdentity:\n    mint: true") {
		t.Error("core.url set but the manifest doesn't declare workloadIdentity.mint")
	}
	var dep map[string]any
	for _, d := range docs(t, on) {
		if d["kind"] == "Deployment" {
			dep = d
		}
	}
	spec := dep["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	c := spec["containers"].([]any)[0].(map[string]any)
	refs := map[string]string{}
	for _, e := range c["env"].([]any) {
		ev := e.(map[string]any)
		if vf, ok := ev["valueFrom"].(map[string]any); ok {
			ref := vf["secretKeyRef"].(map[string]any)
			refs[ev["name"].(string)] = ref["name"].(string) + "/" + ref["key"].(string)
		}
	}
	if refs["BOOTH_WORKLOAD_MINT_URL"] != "booth-workload-minting-credentials/url" || refs["BOOTH_WORKLOAD_MINT_CREDENTIAL"] != "booth-workload-minting-credentials/credential" {
		t.Errorf("minting env = %v", refs)
	}
	vol := spec["volumes"].([]any)[0].(map[string]any)
	if ed, _ := vol["emptyDir"].(map[string]any); ed["medium"] != "Memory" {
		t.Errorf("sidecar volume = %v, want an in-memory emptyDir", vol)
	}
	if fmt.Sprint(c["volumeMounts"]) != "[map[mountPath:/run/booth-api name:sidecars]]" {
		t.Errorf("mounts = %v", c["volumeMounts"])
	}
}

// ADR 0095: the credential sidecar is taken from booth-core's image by digest, never a tag.
func TestDockerfile_SidecarPinnedByDigest(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^FROM ghcr\.io/projectbooth/credential-sidecar@sha256:[0-9a-f]{64} AS sidecar$`)
	if !re.Match(b) {
		t.Error("Dockerfile doesn't take credential-sidecar from a digest-pinned image")
	}
	if !strings.Contains(string(b), "COPY --from=sidecar /credential-sidecar /credential-sidecar") {
		t.Error("Dockerfile doesn't copy the sidecar binary to /credential-sidecar (config's default)")
	}
}
